package archive

import (
	"errors"
	"io"
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

// Clock rollback must not reset a budget that has already been committed.
func (s *Store) receiptUsageLocked() (string, uint64) {
	day := s.now().UTC().Format(time.DateOnly)
	if usage := s.state.DailyReceipt; usage != nil && usage.Day >= day {
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
