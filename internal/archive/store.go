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

// StagedMessage is a complete synced message that is not visible in the mailbox.
type StagedMessage struct {
	store *Store
	path  string
	size  uint64
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
	state, err = recoverMailboxTail(root, state)
	if err != nil {
		return nil, err
	}
	return &Store{root: root, state: state}, nil
}

// Deliver durably publishes one immutable message and returns its final path.
// It never overwrites an existing archive file.
func (s *Store) Deliver(message io.Reader) (finalPath string, err error) {
	staged, err := s.Stage(message)
	if err != nil {
		return "", err
	}
	defer staged.Discard()
	_, finalPath, err = s.Publish(staged)
	return finalPath, err
}

// Stage writes and syncs a complete message without assigning an archive ID.
func (s *Store) Stage(message io.Reader) (*StagedMessage, error) {
	tmpDir := filepath.Join(s.root, "tmp")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return nil, fmt.Errorf("create temporary archive directory: %w", err)
	}
	file, err := os.CreateTemp(tmpDir, ".message-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary message: %w", err)
	}
	path := file.Name()
	succeeded := false
	defer func() {
		_ = file.Close()
		if !succeeded {
			_ = os.Remove(path)
		}
	}()

	written, err := io.Copy(file, message)
	if err != nil {
		return nil, fmt.Errorf("write message: %w", err)
	}
	if err := file.Sync(); err != nil {
		return nil, fmt.Errorf("sync message: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close message: %w", err)
	}
	succeeded = true
	return &StagedMessage{store: s, path: path, size: uint64(written)}, nil
}

// Open opens the unpublished staged content for policy inspection.
func (m *StagedMessage) Open() (*os.File, error) {
	if m == nil || m.path == "" {
		return nil, errors.New("staged message is unavailable")
	}
	return os.Open(m.path)
}

// Size returns the exact stored octet count.
func (m *StagedMessage) Size() uint64 {
	if m == nil {
		return 0
	}
	return m.size
}

// Discard removes unpublished staging data. It is safe after Publish.
func (m *StagedMessage) Discard() error {
	if m == nil || m.path == "" {
		return nil
	}
	err := os.Remove(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Publish assigns the next ID and atomically makes a staged message visible.
func (s *Store) Publish(staged *StagedMessage) (id uint64, finalPath string, err error) {
	if staged == nil || staged.store != s || staged.path == "" {
		return 0, "", errors.New("staged message does not belong to this store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state.LastID == math.MaxUint64 {
		return 0, "", errors.New("message ID space exhausted")
	}
	nextID := s.state.LastID + 1
	relativePath, err := messageRelativePath(nextID)
	if err != nil {
		return 0, "", err
	}
	finalPath = filepath.Join(s.root, relativePath)
	finalDir := filepath.Dir(finalPath)
	if err := os.MkdirAll(finalDir, 0o700); err != nil {
		return 0, "", fmt.Errorf("create archive directory: %w", err)
	}
	if staged.size > math.MaxUint64-s.state.TotalOctets {
		return 0, "", errors.New("mailbox octet count overflow")
	}
	if err := os.Link(staged.path, finalPath); err != nil {
		return 0, "", fmt.Errorf("publish message without overwrite: %w", err)
	}
	if err := syncDirectory(finalDir); err != nil {
		return 0, "", err
	}

	nextState := mailboxState{
		Version:     mailboxStateVersion,
		LastID:      nextID,
		TotalOctets: s.state.TotalOctets + staged.size,
	}
	if err := writeMailboxState(s.root, nextState); err != nil {
		return 0, "", err
	}
	s.state = nextState
	_ = staged.Discard()
	return nextID, finalPath, nil
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
