package archive

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
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
	if store.receipt.ChargedBytes != 8192 {
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
	if reopened.receipt.Day != "2026-10-21" || reopened.receipt.ChargedBytes != 4096 {
		t.Fatalf("new day accounting: %+v", reopened.receipt)
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
	if store.reservedBytes != 0 || store.receipt != nil {
		t.Fatal("failed staging consumed committed budget")
	}
	if _, err := store.Deliver(strings.NewReader(strings.Repeat("x", 8192))); err != nil {
		t.Fatal(err)
	}
	if store.receipt.ChargedBytes != 8192 {
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
	if store.receipt.Day != "2026-10-21" || store.receipt.ChargedBytes != 8192 {
		t.Fatalf("midnight budget: %+v", store.receipt)
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

func TestReceiptFileIsOutsideGitAndSurvivesHEADRollback(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := NewWithOptions(root, Options{MaxDailyBytes: 8192})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	initial := store.headID
	for range 2 {
		if _, err := store.Deliver(strings.NewReader("body")); err != nil {
			t.Fatal(err)
		}
	}
	if store.receiptPath != root+".rate-limit.json" {
		t.Fatalf("unexpected receipt path: %s", store.receiptPath)
	}
	raw, err := os.ReadFile(store.receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	var usage dailyReceipt
	if err := json.Unmarshal(raw, &usage); err != nil || usage.ChargedBytes != 8192 {
		t.Fatalf("file usage = %s, %v", raw, err)
	}
	info, err := os.Stat(store.receiptPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("receipt permissions: %v/%v", info, err)
	}
	state, err := store.git(nil, "show", "HEAD:state")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(state, []byte("daily_receipt")) || bytes.Contains(state, []byte("charged_bytes")) {
		t.Fatalf("quota leaked into Git state: %s", state)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// Deliberately simulate an administrator restoring an older HEAD. Only the
	// budget assertion matters here; production history must remain append-only.
	if _, err := store.git(nil, "update-ref", "refs/heads/main", initial); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewWithOptions(root, Options{MaxDailyBytes: 8192})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.CheckReceive(); !errors.Is(err, ErrDailyLimit) {
		t.Fatalf("HEAD rollback reset external quota: %v", err)
	}
}

func TestFailedGitPublicationKeepsDurableBudgetCharge(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := NewWithOptions(root, Options{MaxDailyBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	head := store.headID
	lock := filepath.Join(root, "refs", "heads", "main.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Deliver(strings.NewReader("failed")); err == nil {
		t.Fatal("published despite ref lock")
	}
	if store.headID != head || store.receipt.ChargedBytes != 4096 || store.reservedBytes != 0 {
		t.Fatal("failed publication lost charge or left reservation")
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewWithOptions(root, Options{MaxDailyBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if reopened.Snapshot().Count() != 0 {
		t.Fatal("failed publication changed mailbox")
	}
	if err := reopened.CheckReceive(); !errors.Is(err, ErrDailyLimit) {
		t.Fatalf("failed publication charge was not durable: %v", err)
	}
}

func TestReceiptFileFailurePreventsGitPublication(t *testing.T) {
	t.Parallel()
	store, err := NewWithOptions(t.TempDir(), Options{MaxDailyBytes: 8192})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	head := store.headID
	// A directory at the target makes atomic replacement fail deterministically,
	// including when tests run as root and chmod cannot simulate a write failure.
	if err := os.Mkdir(store.receiptPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Deliver(strings.NewReader("blocked")); err == nil {
		t.Fatal("published without persisting the charge")
	}
	if store.headID != head || store.Snapshot().Count() != 0 || store.reservedBytes != 0 {
		t.Fatal("quota file error changed Git state or leaked reservation")
	}
	if err := store.CheckReceive(); err == nil {
		t.Fatal("continued after uncertain quota file update")
	}
}

func TestGitBackedReceiptTransfersOnceToExternalFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := testNew(t, root)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Now().UTC().Format(time.DateOnly)
	legacy := store.state
	legacy.DailyReceipt = &dailyReceipt{Day: day, ChargedBytes: 8192}
	if err := store.commitChanges("mail: legacy quota fixture", legacy, map[string][]byte{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewWithOptions(root, Options{MaxDailyBytes: 12288})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if reopened.receipt == nil || reopened.receipt.ChargedBytes != 8192 || reopened.state.DailyReceipt != nil {
		t.Fatal("legacy quota was not transferred")
	}
	if _, err := reopened.Deliver(strings.NewReader("last")); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := NewWithOptions(root, Options{MaxDailyBytes: 12288})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = again.Close() })
	if again.receipt.ChargedBytes != 12288 {
		t.Fatal("external usage was replaced with old Git usage")
	}
	if err := again.CheckReceive(); !errors.Is(err, ErrDailyLimit) {
		t.Fatalf("transfer reset budget: %v", err)
	}
}

func TestInvalidReceiptFileRejectsStartup(t *testing.T) {
	t.Parallel()
	for _, content := range []string{"", `{}`, `{"day":"bad","charged_bytes":1}`, `{"day":"2026-10-20","charged_bytes":0}`, `{"day":"2026-10-20","charged_bytes":1} {}`} {
		root := t.TempDir()
		if err := os.WriteFile(root+".rate-limit.json", []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if store, err := New(root); err == nil {
			_ = store.Close()
			t.Fatalf("accepted invalid receipt file %q", content)
		}
	}
}
