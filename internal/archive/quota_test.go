package archive

import (
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReceiptBudgetPersistsAcrossDeleteRestartAndClockRollback(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := NewWithOptions(root, Options{MaxDailyBytes: 8192})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 10, 20, 23, 59, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	for range 2 {
		if _, err := store.Deliver(strings.NewReader("tiny")); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.CheckReceive(); !errors.Is(err, ErrDailyLimit) {
		t.Fatalf("full budget admission: %v", err)
	}
	if err := store.Delete([]uint64{1, 2}); err != nil {
		t.Fatal(err)
	}
	if store.state.DailyReceipt.ChargedBytes != 8192 {
		t.Fatal("delete refunded receipt budget")
	}
	reopened, err := NewWithOptions(root, Options{MaxDailyBytes: 8192})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	reopened.now = func() time.Time { return now }
	if _, err := reopened.Stage(strings.NewReader("blocked")); !errors.Is(err, ErrDailyLimit) {
		t.Fatalf("restart reset budget: %v", err)
	}
	now = now.Add(-24 * time.Hour)
	if err := reopened.CheckReceive(); !errors.Is(err, ErrDailyLimit) {
		t.Fatalf("clock rollback reset budget: %v", err)
	}
	now = now.Add(48 * time.Hour)
	if _, err := reopened.Deliver(strings.NewReader("next day")); err != nil {
		t.Fatal(err)
	}
	if reopened.state.DailyReceipt.Day != "2026-10-21" || reopened.state.DailyReceipt.ChargedBytes != 4096 {
		t.Fatalf("new day accounting: %+v", reopened.state.DailyReceipt)
	}
}

func TestConcurrentStagingReservationsAndDiscard(t *testing.T) {
	t.Parallel()
	store, err := NewWithOptions(t.TempDir(), Options{MaxDailyBytes: 8192})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	results := make(chan Staged, 20)
	failures := make(chan error, 20)
	var work sync.WaitGroup
	for range 20 {
		work.Add(1)
		go func() {
			defer work.Done()
			staged, err := store.Stage(strings.NewReader("tiny"))
			if err != nil {
				failures <- err
			} else {
				results <- staged
			}
		}()
	}
	work.Wait()
	close(results)
	close(failures)
	var staged []Staged
	for value := range results {
		staged = append(staged, value)
	}
	for err := range failures {
		if !errors.Is(err, ErrDailyLimit) {
			t.Fatal(err)
		}
	}
	if len(staged) != 2 || store.reservedBytes != 8192 {
		t.Fatalf("concurrent reservations: %d/%d", len(staged), store.reservedBytes)
	}
	for _, value := range staged {
		if err := value.Discard(); err != nil {
			t.Fatal(err)
		}
		if err := value.Discard(); err != nil {
			t.Fatal(err)
		}
	}
	if store.reservedBytes != 0 {
		t.Fatal("discard leaked or double-released reservations")
	}
	if _, err := store.Stage(strings.NewReader(strings.Repeat("x", 8193))); !errors.Is(err, ErrDailyLimit) {
		t.Fatalf("oversize stream: %v", err)
	}
	if store.reservedBytes != 0 || store.state.DailyReceipt != nil {
		t.Fatal("failed staging consumed committed budget")
	}
	if _, err := store.Deliver(strings.NewReader(strings.Repeat("x", 8192))); err != nil {
		t.Fatal(err)
	}
	if store.state.DailyReceipt.ChargedBytes != 8192 {
		t.Fatal("large message charged incorrectly")
	}
}

func TestStagingAcrossMidnightUsesCurrentBudgetAndCannotPublishTwice(t *testing.T) {
	t.Parallel()
	store, err := NewWithOptions(t.TempDir(), Options{MaxDailyBytes: 8192})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 10, 20, 23, 59, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	staged, err := store.Stage(strings.NewReader("old day"))
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Discard()
	now = now.Add(time.Minute)
	if _, err := store.Deliver(strings.NewReader("new day")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Publish(staged); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Publish(staged); err == nil {
		t.Fatal("republished discarded stage")
	}
	if store.state.DailyReceipt.Day != "2026-10-21" || store.state.DailyReceipt.ChargedBytes != 8192 {
		t.Fatalf("midnight budget: %+v", store.state.DailyReceipt)
	}
}

func TestFailedStageReleasesReservation(t *testing.T) {
	t.Parallel()
	store, err := NewWithOptions(t.TempDir(), Options{MaxDailyBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.Stage(io.MultiReader(strings.NewReader("partial"), errorReader{})); err == nil {
		t.Fatal("accepted failed stream")
	}
	if err := store.CheckReceive(); err != nil {
		t.Fatalf("failed stream leaked reservation: %v", err)
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
