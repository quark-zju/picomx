// Package archive stores complete RFC 5322 messages in a bare Git repository.
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
	"time"
)

// Store writes immutable messages under deterministic, sequential ID paths.
type Store struct {
	root                    string
	mu                      sync.Mutex
	state                   mailboxState
	headID                  string
	failure                 error
	refName                 string
	gitMu                   sync.Mutex
	readMu                  sync.Mutex
	readClosed              bool
	readIdleTimer           *time.Timer
	readIdleGeneration      uint64
	readIdleTimeout         time.Duration
	importer                *gitProcess
	reader                  *gitProcess
	importError             error
	closed                  bool
	idleTimer               *time.Timer
	idleGeneration          uint64
	idleTimeout             time.Duration
	cache                   *directoryCache
	maxDailyBytes           uint64
	receiptPath             string
	receipt                 *dailyReceipt
	reservedBytes           uint64
	now                     func() time.Time
	maintenanceRequests     chan struct{}
	commitsSinceMaintenance uint64
	lastMaintenanceHead     string
}

// StagedMessage is a complete synced message that is not visible in the mailbox.
type StagedMessage struct {
	store    *Store
	path     string
	size     uint64
	reserved uint64
	done     bool
}

// Staged is the protocol-neutral unpublished message capability.
type Staged interface {
	Open() (io.ReadCloser, error)
	Size() uint64
	Discard() error
}

// New opens the single-writer archive rooted at root.
func New(root string) (*Store, error) { return NewWithOptions(root, Options{}) }

func NewWithOptions(root string, options Options) (*Store, error) {
	if options.MaxDailyBytes == 0 {
		options.MaxDailyBytes = DefaultMaxDailyBytes
	}
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("message archive root is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, err
	}
	store := &Store{root: absolute, receiptPath: absolute + ".rate-limit.json", cache: newDirectoryCache(directoryCacheCapacity), maxDailyBytes: options.MaxDailyBytes, now: time.Now, maintenanceRequests: make(chan struct{}, 1)}
	if err := store.initializeGit(); err != nil {
		_ = store.Close()
		return nil, err
	}
	if err := store.loadReceipt(); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

// Deliver durably publishes one immutable message and returns its path within the committed Git tree.
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
func (s *Store) Stage(message io.Reader) (Staged, error) {
	s.mu.Lock()
	if s.failure != nil {
		err := s.failure
		s.mu.Unlock()
		return nil, err
	}
	if !s.canReserveLocked(minimumReceiptCharge) {
		s.mu.Unlock()
		return nil, ErrDailyLimit
	}
	staged := &StagedMessage{store: s, reserved: minimumReceiptCharge}
	s.reservedBytes += minimumReceiptCharge
	s.mu.Unlock()
	succeeded := false
	defer func() {
		if !succeeded {
			_ = staged.Discard()
		}
	}()

	tmpDir := filepath.Join(s.root, "tmp")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return nil, fmt.Errorf("create temporary archive directory: %w", err)
	}
	file, err := os.CreateTemp(tmpDir, ".message-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary message: %w", err)
	}
	path := file.Name()
	staged.path = path
	defer func() {
		_ = file.Close()
		if !succeeded {
			_ = os.Remove(path)
		}
	}()

	written, err := io.Copy(file, &quotaReader{source: message, message: staged})
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
	staged.size = uint64(written)
	return staged, nil
}

// Open opens the unpublished staged content for policy inspection.
func (m *StagedMessage) Open() (io.ReadCloser, error) {
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
	if m == nil {
		return nil
	}
	m.store.mu.Lock()
	defer m.store.mu.Unlock()
	return m.discardLocked()
}

func (m *StagedMessage) removeFile() error {
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
func (s *Store) Publish(staged Staged) (id uint64, finalPath string, err error) {
	message, ok := staged.(*StagedMessage)
	if !ok || message == nil || message.store != s || message.path == "" {
		return 0, "", errors.New("staged message does not belong to this store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.failure != nil {
		return 0, "", s.failure
	}
	if message.done {
		return 0, "", errors.New("staged message is no longer available")
	}
	if !s.canReserveLocked(0) {
		return 0, "", ErrDailyLimit
	}
	if s.state.LastID == math.MaxUint64 {
		return 0, "", errors.New("message ID space exhausted")
	}
	nextID := s.state.LastID + 1
	relativePath, err := messagePath(nextID, message.size)
	if err != nil {
		return 0, "", err
	}
	finalPath = filepath.ToSlash(relativePath)
	if message.size > math.MaxUint64-s.state.TotalOctets {
		return 0, "", errors.New("mailbox octet count overflow")
	}

	nextState := mailboxState{
		Version:     mailboxStateVersion,
		LastID:      nextID,
		TotalOctets: s.state.TotalOctets + message.size,
		Directories: cloneDirectoryCounts(s.state.Directories),
	}
	nextState.Directories.add(filepath.ToSlash(filepath.Dir(relativePath)))
	nextState.index, err = buildDirectoryIndex(nextState.Directories, nextState.LastID)
	if err != nil {
		return 0, "", err
	}
	// Git receives the staged bytes directly; no working copy is published.
	content, err := os.ReadFile(message.path)
	if err != nil {
		return 0, "", err
	}
	// Persist the charge first: a crash or uncertain Git result may overcount
	// this attempt, but can never publish mail without a durable budget charge.
	day, used := s.receiptUsageLocked()
	usage := &dailyReceipt{Day: day, ChargedBytes: used + message.reserved}
	if err := s.writeReceipt(usage); err != nil {
		s.failure = fmt.Errorf("persist receipt budget: %w", err)
		return 0, "", s.failure
	}
	s.receipt = usage
	_ = message.discardLocked()
	if err := s.commitChanges(fmt.Sprintf("mail: receive %d", nextID), nextState,
		map[string][]byte{filepath.ToSlash(relativePath): content}, nil); err != nil {
		return 0, "", err
	}
	s.state = nextState
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
