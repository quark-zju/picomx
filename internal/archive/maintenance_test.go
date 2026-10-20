package archive

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"
)

func objectCounts(t *testing.T, store *Store) map[string]uint64 {
	t.Helper()
	out, err := store.git(nil, "count-objects", "-v")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]uint64{}
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		counts[key], err = strconv.ParseUint(value, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
	}
	return counts
}

func TestMaintenancePacksLooseObjectsAndKeepsHistoricalMail(t *testing.T) {
	t.Parallel()
	store, err := testNew(t, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for range 12 {
		if _, err := store.Deliver(strings.NewReader("historical body")); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := store.Snapshot()
	if err := store.Delete([]uint64{1}); err != nil {
		t.Fatal(err)
	}
	head := store.headID
	before := objectCounts(t, store)
	file, err := store.OpenAt(snapshot, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Maintain(context.Background()); !errors.Is(err, ErrMaintenanceBusy) {
		t.Fatalf("maintenance did not yield to active reader: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := objectCounts(t, store)
	if before["count"] == 0 || after["count"] >= before["count"] || after["in-pack"] == 0 {
		t.Fatalf("objects not packed: %v -> %v", before, after)
	}
	if store.headID != head || store.importer != nil || store.reader != nil {
		t.Fatal("maintenance moved HEAD or left a stale Git process")
	}
	file, err = store.OpenAt(snapshot, 1)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(file)
	closeErr := file.Close()
	if err != nil || closeErr != nil || string(data) != "historical body" {
		t.Fatalf("history read after packing: %q, %v/%v", data, err, closeErr)
	}
	if _, err := store.Deliver(strings.NewReader("after maintenance")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.git(nil, "fsck", "--strict"); err != nil {
		t.Fatal(err)
	}
}

func TestPeriodicMaintenanceRunsAndCanBeCancelled(t *testing.T) {
	t.Parallel()
	store, err := testNew(t, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Deliver(strings.NewReader("body")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		store.RunMaintenance(ctx, 10*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		store.mu.Lock()
		packed := store.lastMaintenanceHead == store.headID
		store.mu.Unlock()
		if packed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("periodic maintenance did not run")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("maintenance did not stop")
	}
}

func TestReceiptRequestsMaintenanceAfterCommitThreshold(t *testing.T) {
	t.Parallel()
	store, err := testNew(t, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store.gitMu.Lock()
	store.commitsSinceMaintenance = maintenanceCommitThreshold - 1
	store.gitMu.Unlock()
	if _, err := store.Deliver(strings.NewReader("trigger")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-store.maintenanceRequests:
	default:
		t.Fatal("receipt did not request maintenance")
	}
}
