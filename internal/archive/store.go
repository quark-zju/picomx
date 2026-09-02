// Package archive stores complete RFC 5322 messages in an append-only tree.
package archive

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Store writes immutable messages under deterministic, sequential ID paths.
type Store struct {
	root  string
	mu    sync.Mutex
	state mailboxState
}

// New opens the single-writer archive rooted at root.
func New(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("message archive root is required")
	}
	state, err := readMailboxState(root)
	if err != nil {
		return nil, err
	}
	return &Store{root: root, state: state}, nil
}

// Deliver durably publishes one immutable message and returns its final path.
// It never overwrites an existing archive file.
func (s *Store) Deliver(message io.Reader) (finalPath string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state.LastID == math.MaxUint64 {
		return "", errors.New("message ID space exhausted")
	}
	nextID := s.state.LastID + 1
	relativePath, err := messageRelativePath(nextID)
	if err != nil {
		return "", err
	}
	finalPath = filepath.Join(s.root, relativePath)
	finalDir := filepath.Dir(finalPath)
	tmpDir := filepath.Join(s.root, "tmp")
	for _, dir := range []string{finalDir, tmpDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("create archive directory: %w", err)
		}
	}

	file, err := os.CreateTemp(tmpDir, ".message-*")
	if err != nil {
		return "", fmt.Errorf("create temporary message: %w", err)
	}
	tmpPath := file.Name()
	defer func() {
		_ = file.Close()
		_ = os.Remove(tmpPath)
	}()

	written, err := io.Copy(file, message)
	if err != nil {
		return "", fmt.Errorf("write message: %w", err)
	}
	if uint64(written) > math.MaxUint64-s.state.TotalOctets {
		return "", errors.New("mailbox octet count overflow")
	}
	if err := file.Sync(); err != nil {
		return "", fmt.Errorf("sync message: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close message: %w", err)
	}
	if err := os.Link(tmpPath, finalPath); err != nil {
		return "", fmt.Errorf("publish message without overwrite: %w", err)
	}
	if err := syncDirectory(finalDir); err != nil {
		return "", err
	}

	nextState := mailboxState{
		Version:     mailboxStateVersion,
		LastID:      nextID,
		TotalOctets: s.state.TotalOctets + uint64(written),
	}
	if err := writeMailboxState(s.root, nextState); err != nil {
		return "", err
	}
	s.state = nextState
	return finalPath, nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open archive directory for sync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync archive directory: %w", err)
	}
	return nil
}
