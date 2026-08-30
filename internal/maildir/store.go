// Package maildir stores complete RFC 5322 messages in monthly Maildirs.
package maildir

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

// Store writes messages beneath Root/YYYY/MM/{tmp,new,cur}.
type Store struct {
	root     string
	hostname string
	now      func() time.Time
	sequence atomic.Uint64
}

// New returns a mail store rooted at root. Directories are created lazily.
func New(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("maildir root is required")
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

// Deliver durably writes one message and returns its final path. A successful
// return means the file has been renamed into new/ and both it and its parent
// directory have been synchronized.
func (s *Store) Deliver(message io.Reader) (finalPath string, err error) {
	now := s.now().UTC()
	monthDir := filepath.Join(s.root, now.Format("2006"), now.Format("01"))
	for _, name := range []string{"tmp", "new", "cur"} {
		if err := os.MkdirAll(filepath.Join(monthDir, name), 0o700); err != nil {
			return "", fmt.Errorf("create maildir %s: %w", name, err)
		}
	}

	filename := fmt.Sprintf(
		"%d.%d_%d.%s",
		now.UnixNano(),
		os.Getpid(),
		s.sequence.Add(1),
		s.hostname,
	)
	tmpPath := filepath.Join(monthDir, "tmp", filename)
	finalPath = filepath.Join(monthDir, "new", filename)

	file, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create temporary message: %w", err)
	}
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(tmpPath)
		}
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
	if err = os.Rename(tmpPath, finalPath); err != nil {
		return "", fmt.Errorf("publish message: %w", err)
	}
	keep = true

	newDir, openErr := os.Open(filepath.Join(monthDir, "new"))
	if openErr != nil {
		return "", fmt.Errorf("open maildir for sync: %w", openErr)
	}
	defer newDir.Close()
	if err = newDir.Sync(); err != nil {
		return "", fmt.Errorf("sync maildir: %w", err)
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
