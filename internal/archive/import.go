package archive

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const gitIdleTimeout = 30 * time.Second

// stderr is drained by os/exec concurrently, including while the importer waits
// for another request. Bound it and synchronize access to diagnostics.
type gitDiagnostics struct {
	mu   sync.Mutex
	data []byte
}

func (d *gitDiagnostics) Write(data []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	remaining := 4096 - len(d.data)
	if remaining > len(data) {
		remaining = len(data)
	}
	d.data = append(d.data, data[:remaining]...)
	return len(data), nil
}

func (d *gitDiagnostics) String() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return strings.TrimSpace(string(d.data))
}

type gitProcess struct {
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	input       *bufio.Writer
	output      *bufio.Reader
	diagnostics gitDiagnostics
	nextMark    uint64
	timeout     time.Duration
}

func (s *Store) importerLocked() (*gitProcess, error) {
	s.idleGeneration++
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
	if s.closed {
		return nil, errors.New("archive is closed")
	}
	if s.importError != nil {
		return nil, s.importError
	}
	if s.importer != nil {
		return s.importer, nil
	}
	cmd := s.gitCommand("fast-import", "--quiet")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	process := &gitProcess{cmd: cmd, stdin: stdin, input: bufio.NewWriter(stdin), output: bufio.NewReader(stdout)}
	cmd.Stderr = &process.diagnostics
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, err
	}
	s.importer = process
	return process, nil
}

func (s *Store) armIdleLocked() {
	if s.importer == nil {
		return
	}
	if s.idleTimer != nil {
		s.idleTimer.Stop()
	}
	s.idleGeneration++
	generation := s.idleGeneration
	delay := s.idleTimeout
	if delay <= 0 {
		delay = gitIdleTimeout
	}
	s.idleTimer = time.AfterFunc(delay, func() {
		s.gitMu.Lock()
		defer s.gitMu.Unlock()
		if s.idleGeneration != generation {
			return
		}
		if err := s.closeImporterLocked(); err != nil {
			s.importError = err
		}
	})
}

// All completed transactions have already checkpointed. EOF can therefore close
// an idle process without publishing any unacknowledged buffered transaction.
func (s *Store) closeImporterLocked() error {
	if s.importer == nil {
		return nil
	}
	process := s.importer
	s.importer = nil
	closeErr := process.stdin.Close()
	waitErr := process.watch(process.cmd.Wait)
	if waitErr != nil {
		return fmt.Errorf("close git fast-import: %w: %s", waitErr, process.diagnostics.String())
	}
	return closeErr
}

// Kill before closing stdin on errors: EOF alone could publish a partial or
// previously rejected transaction at fast-import's final ref-update phase.
func (s *Store) abortImporterLocked(cause error) error {
	if s.importer != nil {
		process := s.importer
		s.importer = nil
		killGit(process.cmd)
		_ = process.stdin.Close()
		_ = process.cmd.Wait()
		cause = fmt.Errorf("%w: %s", cause, process.diagnostics.String())
	}
	s.importError = cause
	return cause
}

// Close ends the idle timer and gracefully stops the optional child process.
func (s *Store) Close() error {
	s.gitMu.Lock()
	s.closed = true
	s.idleGeneration++
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
	writeErr := s.closeImporterLocked()
	s.gitMu.Unlock()
	s.readMu.Lock()
	defer s.readMu.Unlock()
	s.readClosed = true
	s.readIdleGeneration++
	if s.readIdleTimer != nil {
		s.readIdleTimer.Stop()
		s.readIdleTimer = nil
	}
	return errors.Join(writeErr, s.closeReaderLocked())
}

func (process *gitProcess) line() (string, error) {
	line, err := process.output.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(line, "\n"), nil
}

func validObjectID(id string) bool {
	if len(id) != 40 && len(id) != 64 {
		return false
	}
	for _, char := range id {
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func (process *gitProcess) data(source io.Reader, size int64) error {
	if _, err := fmt.Fprintf(process.input, "data %d\n", size); err != nil {
		return err
	}
	if _, err := io.CopyN(process.input, source, size); err != nil {
		return err
	}
	return process.input.WriteByte('\n')
}

// commitChanges uses fast-import directly: no index and no working copy. HEAD is
// the only publication boundary; every commit retains the previous one as parent.
func (s *Store) commitChanges(title string, state mailboxState, changes map[string][]byte, removals []string) error {
	if s.failure != nil {
		return s.failure
	}
	if _, err := buildDirectoryIndex(state.Directories, state.LastID); err != nil {
		return err
	}
	s.gitMu.Lock()
	defer s.gitMu.Unlock()
	defer s.armIdleLocked()
	if current, err := s.head(); (err != nil && s.headID != "") || current != s.headID {
		return errors.New("archive HEAD changed outside picomx; restart required")
	}
	process, err := s.importerLocked()
	if err != nil {
		return err
	}
	// Any protocol error poisons this store, requiring a fresh load of HEAD.
	if err := process.watch(func() error { return s.importCommitLocked(process, title, state, changes, removals) }); err != nil {
		s.failure = s.abortImporterLocked(err)
		return s.failure
	}
	s.commitsSinceMaintenance++
	if s.commitsSinceMaintenance >= maintenanceCommitThreshold {
		select {
		case s.maintenanceRequests <- struct{}{}:
		default:
		}
	}
	return nil
}

func (s *Store) importCommitLocked(process *gitProcess, title string, state mailboxState, changes map[string][]byte, removals []string) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	changes[mailboxStateName] = append(data, '\n')
	process.nextMark++
	if _, err := fmt.Fprintf(process.input, "commit %s\nmark :%d\ncommitter picomx <picomx@localhost> %d +0000\n", s.refName, process.nextMark, time.Now().Unix()); err != nil {
		return err
	}
	if err := process.data(strings.NewReader(title), int64(len(title))); err != nil {
		return err
	}
	if s.headID != "" {
		if _, err := fmt.Fprintf(process.input, "from %s\n", s.headID); err != nil {
			return err
		}
	}
	for path, content := range changes {
		if !validArchivePath(path) {
			return errors.New("invalid archive path")
		}
		if _, err := fmt.Fprintf(process.input, "M 100644 inline %s\n", path); err != nil {
			return err
		}
		if content != nil {
			if err := process.data(bytes.NewReader(content), int64(len(content))); err != nil {
				return err
			}
		} else {
			message, parseErr := messageFromPath(path)
			if parseErr != nil {
				return parseErr
			}
			legacyPath, _ := messageRelativePath(message.ID)
			file, err := os.Open(filepath.Join(s.root, legacyPath))
			if err != nil {
				return err
			}
			info, err := file.Stat()
			if err == nil {
				err = process.data(file, info.Size())
			}
			_ = file.Close()
			if err != nil {
				return err
			}
		}
	}
	for _, path := range removals {
		if !validArchivePath(path) {
			return errors.New("invalid archive path")
		}
		if _, err := fmt.Fprintf(process.input, "D %s\n", path); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(process.input, "\nget-mark :%d\n", process.nextMark); err != nil {
		return err
	}
	if err := process.input.Flush(); err != nil {
		return err
	}
	next, err := process.line()
	if err != nil {
		return err
	}
	if !validObjectID(next) {
		return errors.New("invalid commit object ID from git")
	}
	if _, err := fmt.Fprintf(process.input, "checkpoint\nprogress %s\n", next); err != nil {
		return err
	}
	if err := process.input.Flush(); err != nil {
		return err
	}
	progress, err := process.line()
	if err != nil {
		return err
	}
	if progress != "progress "+next {
		return errors.New("invalid git checkpoint response")
	}
	// A progress response confirms the checkpoint finished, but fast-import may
	// have warned and skipped a failed ref update. Verify actual publication.
	current, err := s.head()
	if err != nil {
		return err
	}
	if current != next {
		return errors.New("git checkpoint did not publish the expected HEAD")
	}
	s.headID = next
	return nil
}

func validArchivePath(path string) bool {
	if path == mailboxStateName || path == ".gitignore" || path == ".gitattributes" {
		return true
	}
	_, err := messageFromPath(path)
	return err == nil
}
