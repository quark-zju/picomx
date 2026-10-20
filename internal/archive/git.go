package archive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func (s *Store) gitCommand(args ...string) *exec.Cmd {
	options := []string{"--git-dir=" + s.root,
		"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
		"-c", "core.attributesFile=/dev/null", "-c", "core.autocrlf=false",
		"-c", "commit.gpgSign=false", "-c", "gc.auto=0", "-c", "maintenance.auto=false",
		"-c", "core.fsync=committed", "-c", "core.fsyncMethod=fsync"}
	cmd := exec.Command("git", append(options, args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, "GIT_") {
			cmd.Env = append(cmd.Env, variable)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_NO_REPLACE_OBJECTS=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=picomx", "GIT_AUTHOR_EMAIL=picomx@localhost",
		"GIT_COMMITTER_NAME=picomx", "GIT_COMMITTER_EMAIL=picomx@localhost")

	return cmd
}

func (s *Store) git(input io.Reader, args ...string) ([]byte, error) {
	return s.runGit(context.Background(), gitOperationTimeout, input, args...)
}

func (s *Store) runGit(ctx context.Context, timeout time.Duration, input io.Reader, args ...string) ([]byte, error) {
	cmd := s.gitCommand(args...)
	cmd.Stdin = input
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	if err := watchGit(ctx, cmd, timeout, cmd.Wait); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func (s *Store) head() (string, error) {
	out, err := s.git(nil, "rev-parse", "--verify", "HEAD")
	return strings.TrimSpace(string(out)), err
}

func (s *Store) initializeGit() error {
	version, err := s.git(nil, "version")
	if err != nil {
		return fmt.Errorf("Git 2.36 or newer is required: %w", err)
	}
	fields := strings.Fields(string(version))
	if len(fields) < 3 {
		return errors.New("cannot determine Git version")
	}
	numbers := strings.Split(fields[2], ".")
	if len(numbers) < 2 {
		return errors.New("cannot determine Git version")
	}
	major, majorErr := strconv.Atoi(numbers[0])
	minor, minorErr := strconv.Atoi(numbers[1])
	if majorErr != nil || minorErr != nil || major < 2 || (major == 2 && minor < 36) {
		return errors.New("Git 2.36 or newer is required")
	}
	if _, err := os.Lstat(filepath.Join(s.root, ".git")); err == nil {
		return errors.New("archive has a working tree; clone it with git clone --bare to a new directory and set archive-root to that directory")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := os.Lstat(filepath.Join(s.root, "HEAD")); errors.Is(err, os.ErrNotExist) {
		if err := s.requireUninitializedArchive(); err != nil {
			return err
		}
		if _, err := s.git(nil, "init", "--bare", "--template=", "--initial-branch=main"); err != nil {
			return err
		}
		if err := syncDirectory(s.root); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	bare, err := s.git(nil, "rev-parse", "--is-bare-repository")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(bare)) != "true" {
		return errors.New("archive must be a bare Git repository")
	}
	if err := os.MkdirAll(filepath.Join(s.root, "tmp"), 0o700); err != nil {
		return err
	}

	head, err := s.head()
	if err != nil {
		// A missing HEAD is legitimate only for a new, unborn branch.
		if err := s.requireUninitializedArchive(); err != nil {
			return err
		}
		ref, refErr := s.git(nil, "symbolic-ref", "HEAD")
		if refErr != nil {
			return err
		}
		refs, refErr := s.git(nil, "for-each-ref", "--format=%(refname)", strings.TrimSpace(string(ref)))
		if refErr != nil || len(refs) != 0 {
			return err
		}
		head = ""
	}
	ref, err := s.git(nil, "symbolic-ref", "HEAD")
	if err != nil {
		return errors.New("archive HEAD must point to a branch")
	}
	s.refName = strings.TrimSpace(string(ref))
	if !strings.HasPrefix(s.refName, "refs/heads/") || strings.ContainsAny(s.refName, "\r\n") {
		return errors.New("invalid archive branch")
	}
	s.headID = head
	if head == "" {
		state, err := readMailboxState(s.root)
		if err != nil {
			return err
		}
		state, err = recoverMailboxTail(s.root, state)
		if err != nil {
			return err
		}
		changes := map[string][]byte{}
		state.Directories = directoryCounts{}
		var total uint64
		for id := uint64(1); id != 0 && id <= state.LastID; id++ {
			info, err := statMessage(s.root, id)
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return errors.New("legacy message is not regular")
			}
			size := uint64(info.Size())
			if size > ^uint64(0)-total {
				return errors.New("legacy archive octet overflow")
			}
			total += size
			path, _ := messagePath(id, size)
			changes[path] = nil
			state.Directories.add(filepath.ToSlash(filepath.Dir(path)))
		}
		if total != state.TotalOctets {
			return errors.New("legacy archive octets do not match state")
		}
		state.index, err = buildDirectoryIndex(state.Directories, state.LastID)
		if err != nil {
			return err
		}
		s.state = state
		if err := s.commitChanges("mail: initialize archive", state, changes, nil); err != nil {
			return err
		}
	} else {
		reader, err := s.readBlob(head, mailboxStateName)
		if err != nil {
			return fmt.Errorf("read committed mailbox state: %w", err)
		}
		data, readErr := io.ReadAll(reader)
		if err := errors.Join(readErr, reader.Close()); err != nil {
			return err
		}
		state, err := decodeMailboxState(bytes.NewReader(data))
		if err != nil {
			return err
		}
		s.state = state
	}

	// Committed state is authoritative. Rebuilding the directory-count index is
	// O(directories); mail objects are parsed and checked on demand, not scanned
	// at every restart. picomx is the only supported writer of this repository.
	return nil
}

// Only the missing-HEAD recovery path needs this check. Normal startup trusts
// committed state and does not walk the object database or enumerate mail.
func (s *Store) requireUninitializedArchive() error {
	found := errors.New("archive has existing Git data; restore HEAD instead of importing legacy mail")
	for _, name := range []string{"objects", "refs", "logs"} {
		root := filepath.Join(s.root, name)
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if errors.Is(err, os.ErrNotExist) && path == root {
				return nil
			}
			if err != nil {
				return err
			}
			if !entry.IsDir() {
				return found
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if info, err := os.Stat(filepath.Join(s.root, "packed-refs")); err == nil {
		if info.Size() != 0 {
			return found
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
