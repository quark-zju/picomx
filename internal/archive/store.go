// Package archive stores complete RFC 5322 messages in an append-only tree.
package archive

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// Store writes immutable messages beneath Root/YYYY/MM/.
type Store struct {
	root     string
	hostname string
	now      func() time.Time
	sequence atomic.Uint64
}

// New returns an archive rooted at root. Directories are created lazily.
func New(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("message archive root is required")
	}
	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("get hostname: %w", err)
	}
	return newStore(root, hostname, time.Now), nil
}

func newStore(root, hostname string, now func() time.Time) *Store {
	return &Store{
		root:     root,
		hostname: safeHostname(hostname),
		now:      now,
	}
}

// Deliver durably publishes one immutable message and returns its final path.
// It never overwrites an existing archive file.
func (s *Store) Deliver(message io.Reader) (finalPath string, err error) {
	now := s.now().UTC()
	monthDir := filepath.Join(s.root, now.Format("2006"), now.Format("01"))
	tmpDir := filepath.Join(s.root, "tmp")
	for _, dir := range []string{monthDir, tmpDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("create archive directory: %w", err)
		}
	}

	filename := fmt.Sprintf(
		"%d.%d_%d.%s.eml",
		now.UnixNano(),
		os.Getpid(),
		s.sequence.Add(1),
		s.hostname,
	)
	tmpPath := filepath.Join(tmpDir, filename)
	finalPath = filepath.Join(monthDir, filename)

	file, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create temporary message: %w", err)
	}
	defer func() {
		_ = file.Close()
		_ = os.Remove(tmpPath)
	}()

	if _, err = io.Copy(file, message); err != nil {
		return "", fmt.Errorf("write message: %w", err)
	}
	if err = file.Sync(); err != nil {
		return "", fmt.Errorf("sync message: %w", err)
	}
	if err = file.Close(); err != nil {
		return "", fmt.Errorf("close message: %w", err)
	}
	// Link, unlike Rename, fails if finalPath already exists. Both directories
	// live under the same archive root and therefore on the same filesystem.
	if err = os.Link(tmpPath, finalPath); err != nil {
		return "", fmt.Errorf("publish message without overwrite: %w", err)
	}

	finalDir, openErr := os.Open(monthDir)
	if openErr != nil {
		return "", fmt.Errorf("open archive for sync: %w", openErr)
	}
	defer finalDir.Close()
	if err = finalDir.Sync(); err != nil {
		return "", fmt.Errorf("sync archive: %w", err)
	}
	return finalPath, nil
}

func safeHostname(hostname string) string {
	if hostname == "" {
		return "localhost"
	}
	var b strings.Builder
	for _, char := range hostname {
		switch {
		case char >= 'a' && char <= 'z':
			b.WriteRune(char)
		case char >= 'A' && char <= 'Z':
			b.WriteRune(char)
		case char >= '0' && char <= '9':
			b.WriteRune(char)
		case char == '-' || char == '.':
			b.WriteRune(char)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
