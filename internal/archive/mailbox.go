package archive

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrNoSuchMessage reports an ID outside the current append-only mailbox.
var ErrNoSuchMessage = errors.New("no such message")

// Snapshot is the constant-size mailbox view captured by a POP session.
type Snapshot struct {
	LastID      uint64
	TotalOctets uint64
}

// Snapshot returns the current immutable high-water mark without scanning mail.
func (s *Store) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Snapshot{LastID: s.state.LastID, TotalOctets: s.state.TotalOctets}
}

// Open opens one published regular message by archive ID.
func (s *Store) Open(id uint64) (*os.File, error) {
	path, err := s.messagePath(id)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoSuchMessage
	}
	if err != nil {
		return nil, fmt.Errorf("inspect message %d: %w", id, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("message %d is not a regular file", id)
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoSuchMessage
	}
	if err != nil {
		return nil, fmt.Errorf("open message %d: %w", id, err)
	}
	return file, nil
}

// Size returns the stored POP octet count for one message.
func (s *Store) Size(id uint64) (uint64, error) {
	path, err := s.messagePath(id)
	if err != nil {
		return 0, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, ErrNoSuchMessage
	}
	if err != nil {
		return 0, fmt.Errorf("inspect message %d: %w", id, err)
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("message %d is not a regular file", id)
	}
	return uint64(info.Size()), nil
}

func (s *Store) messagePath(id uint64) (string, error) {
	s.mu.Lock()
	lastID := s.state.LastID
	s.mu.Unlock()
	if id == 0 || id > lastID {
		return "", ErrNoSuchMessage
	}
	relativePath, err := messageRelativePath(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.root, relativePath), nil
}
