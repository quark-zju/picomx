package archive

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

type objectHeader struct {
	oid, kind string
	size      int64
}

// --batch-command (Git >= 2.36) lets one reader answer metadata and contents
// requests for blobs and trees. No new child is needed for a directory miss.
func (s *Store) readerLocked() (*gitProcess, error) {
	s.readIdleGeneration++
	if s.readIdleTimer != nil {
		s.readIdleTimer.Stop()
		s.readIdleTimer = nil
	}
	if s.readClosed {
		return nil, errors.New("archive is closed")
	}
	if s.reader != nil {
		return s.reader, nil
	}
	cmd := s.gitCommand("cat-file", "--batch-command")
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
	s.reader = process
	return process, nil
}

func (s *Store) closeReaderLocked() error {
	if s.reader == nil {
		return nil
	}
	process := s.reader
	s.reader = nil
	closeErr := process.stdin.Close()
	if err := process.watch(process.cmd.Wait); err != nil {
		return fmt.Errorf("close git cat-file: %w: %s", err, process.diagnostics.String())
	}
	return closeErr
}

func (s *Store) abortReaderLocked(cause error) error {
	if s.reader != nil {
		process := s.reader
		s.reader = nil
		killGit(process.cmd)
		_ = process.stdin.Close()
		_ = process.cmd.Wait()
		return fmt.Errorf("%w: %s", cause, process.diagnostics.String())
	}
	return cause
}

func (process *gitProcess) queryObject(command, spec, kind string) (header objectHeader, err error) {
	err = process.watch(func() error {
		var queryErr error
		header, queryErr = process.queryObjectInner(command, spec, kind)
		return queryErr
	})
	return header, err
}

func (process *gitProcess) queryObjectInner(command, spec, kind string) (objectHeader, error) {
	if strings.ContainsAny(spec, "\r\n") {
		return objectHeader{}, errors.New("invalid Git object query")
	}
	if _, err := fmt.Fprintf(process.input, "%s %s\n", command, spec); err != nil {
		return objectHeader{}, err
	}
	if err := process.input.Flush(); err != nil {
		return objectHeader{}, err
	}
	line, err := process.line()
	if err != nil {
		return objectHeader{}, err
	}
	if line == spec+" missing" {
		return objectHeader{}, ErrNoSuchMessage
	}
	fields := strings.Fields(line)
	if len(fields) != 3 || !validObjectID(fields[0]) || fields[1] != kind {
		return objectHeader{}, errors.New("invalid git object header")
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 {
		return objectHeader{}, errors.New("invalid git object size")
	}
	return objectHeader{oid: fields[0], kind: fields[1], size: size}, nil
}

func (s *Store) objectInfo(spec, kind string) (objectHeader, error) {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	defer s.armReadIdleLocked()
	process, err := s.readerLocked()
	if err != nil {
		return objectHeader{}, err
	}
	header, err := process.queryObject("info", spec, kind)
	if err != nil && !errors.Is(err, ErrNoSuchMessage) {
		err = s.abortReaderLocked(err)
	}
	return header, err
}

// readObject leases the response pipe until Close; closing an unread/partial
// object drains its framing so the next request starts at a header boundary.
func (s *Store) readObject(spec, kind string) (*blobReader, error) {
	s.readMu.Lock()
	leased := false
	defer func() {
		if !leased {
			s.armReadIdleLocked()
			s.readMu.Unlock()
		}
	}()
	process, err := s.readerLocked()
	if err != nil {
		return nil, err
	}
	header, err := process.queryObject("contents", spec, kind)
	if errors.Is(err, ErrNoSuchMessage) {
		return nil, err
	}
	if err != nil {
		return nil, s.abortReaderLocked(err)
	}
	leased = true
	return &blobReader{store: s, process: process, remaining: header.size, header: header}, nil
}

func (s *Store) readBlob(commit, path string) (*blobReader, error) {
	return s.readObject(commit+":"+path, "blob")
}

type blobReader struct {
	store      *Store
	process    *gitProcess
	header     objectHeader
	remaining  int64
	closed     bool
	closeError error
}

func (reader *blobReader) Read(buffer []byte) (int, error) {
	if reader.closed {
		return 0, os.ErrClosed
	}
	if len(buffer) == 0 {
		return 0, nil
	}
	if reader.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(buffer)) > reader.remaining {
		buffer = buffer[:reader.remaining]
	}
	var n int
	err := reader.process.watch(func() error { var readErr error; n, readErr = reader.process.output.Read(buffer); return readErr })
	reader.remaining -= int64(n)
	if errors.Is(err, io.EOF) && reader.remaining > 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func (reader *blobReader) Close() error {
	if reader.closed {
		return reader.closeError
	}
	reader.closed = true
	defer reader.store.readMu.Unlock()
	defer reader.store.armReadIdleLocked()
	err := reader.process.watch(func() error {
		_, err := io.CopyN(io.Discard, reader.process.output, reader.remaining)
		if err == nil {
			var end byte
			end, err = reader.process.output.ReadByte()
			if err == nil && end != '\n' {
				err = errors.New("invalid git object terminator")
			}
		}
		return err
	})
	if err != nil {
		reader.closeError = reader.store.abortReaderLocked(err)
	}
	return reader.closeError
}

func (s *Store) armReadIdleLocked() {
	if s.reader == nil || s.readClosed {
		return
	}
	if s.readIdleTimer != nil {
		s.readIdleTimer.Stop()
	}
	s.readIdleGeneration++
	generation := s.readIdleGeneration
	delay := s.readIdleTimeout
	if delay <= 0 {
		delay = gitIdleTimeout
	}
	s.readIdleTimer = time.AfterFunc(delay, func() {
		s.readMu.Lock()
		defer s.readMu.Unlock()
		if s.readIdleGeneration == generation {
			_ = s.closeReaderLocked()
		}
	})
}
