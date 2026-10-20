package archive

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const DefaultMaxDailyBytes = uint64(50 << 20)
const minimumReceiptCharge = uint64(4 << 10)

var ErrDailyLimit = errors.New("daily receipt budget exceeded")

type dailyReceipt struct {
	Day          string `json:"day"`
	ChargedBytes uint64 `json:"charged_bytes"`
}

type Options struct{ MaxDailyBytes uint64 }

// Clock rollback must not reset a budget that has already been persisted.
func (s *Store) receiptUsageLocked() (string, uint64) {
	day := s.now().UTC().Format(time.DateOnly)
	if usage := s.receipt; usage != nil && usage.Day >= day {
		return usage.Day, usage.ChargedBytes
	}
	return day, 0
}

func (s *Store) canReserveLocked(extra uint64) bool {
	_, used := s.receiptUsageLocked()
	return used <= s.maxDailyBytes && s.reservedBytes <= s.maxDailyBytes-used && extra <= s.maxDailyBytes-used-s.reservedBytes
}

// CheckReceive rejects exhausted budgets before SMTP invites DATA. Stage also
// reserves while streaming, so concurrent connections cannot exceed the limit.
func (s *Store) CheckReceive() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure != nil {
		return s.failure
	}
	if !s.canReserveLocked(minimumReceiptCharge) {
		return ErrDailyLimit
	}
	return nil
}

type quotaReader struct {
	source   io.Reader
	message  *StagedMessage
	consumed uint64
}

func (r *quotaReader) Read(buffer []byte) (int, error) {
	n, err := r.source.Read(buffer)
	if n == 0 {
		return n, err
	}
	charge := r.consumed + uint64(n)
	if charge < minimumReceiptCharge {
		charge = minimumReceiptCharge
	}
	s := r.message.store
	s.mu.Lock()
	extra := charge - r.message.reserved
	if !s.canReserveLocked(extra) {
		s.mu.Unlock()
		return 0, ErrDailyLimit
	}
	s.reservedBytes += extra
	r.message.reserved = charge
	s.mu.Unlock()
	r.consumed += uint64(n)
	return n, err
}

func (m *StagedMessage) discardLocked() error {
	if !m.done {
		m.store.reservedBytes -= m.reserved
		m.reserved = 0
		m.done = true
	}
	return m.removeFile()
}

// The receipt file is a sibling of the bare repository, outside both its Git
// tree and its object database. Git cloning or rolling back HEAD cannot reset it.
func (s *Store) loadReceipt() error {
	file, err := os.Open(s.receiptPath)
	if errors.Is(err, os.ErrNotExist) {
		// Read compatibility for the previous Git-backed counter. Transfer once;
		// after this, only the sibling file is authoritative, even across HEAD changes.
		if legacy := s.state.DailyReceipt; legacy != nil {
			if err := s.writeReceipt(legacy); err != nil {
				return err
			}
			s.receipt = legacy
		}
	} else if err != nil {
		return fmt.Errorf("open receipt budget: %w", err)
	} else {
		defer file.Close()
		decoder := json.NewDecoder(file)
		decoder.DisallowUnknownFields()
		var usage dailyReceipt
		if err := decoder.Decode(&usage); err != nil {
			return fmt.Errorf("decode receipt budget: %w", err)
		}
		if err := ensureJSONEOF(decoder); err != nil {
			return err
		}
		if err := validateReceipt(&usage); err != nil {
			return err
		}
		s.receipt = &usage
	}
	s.state.DailyReceipt = nil
	return nil
}

func validateReceipt(usage *dailyReceipt) error {
	if _, err := time.Parse(time.DateOnly, usage.Day); err != nil || usage.ChargedBytes == 0 {
		return errors.New("invalid daily receipt accounting")
	}
	return nil
}

func (s *Store) writeReceipt(usage *dailyReceipt) (err error) {
	dir := filepath.Dir(s.receiptPath)
	file, err := os.CreateTemp(dir, "."+filepath.Base(s.receiptPath)+"-*")
	if err != nil {
		return err
	}
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	if err := json.NewEncoder(file).Encode(usage); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), s.receiptPath); err != nil {
		return err
	}
	return syncDirectory(dir)
}
