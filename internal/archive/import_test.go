package archive

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestImporterExitsOnIdleAndRestartsFromCommittedHEAD(t *testing.T) {
	t.Parallel()
	store, err := testNew(t, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store.gitMu.Lock()
	store.idleTimeout = 20 * time.Millisecond
	store.readMu.Lock()
	store.readIdleTimeout = 20 * time.Millisecond
	store.armReadIdleLocked()
	store.readMu.Unlock()
	store.gitMu.Unlock()
	if _, err := store.Deliver(strings.NewReader("first")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		store.gitMu.Lock()
		idle := store.importer == nil
		store.readMu.Lock()
		idle = idle && store.reader == nil
		store.readMu.Unlock()
		exitErr := store.importError
		store.gitMu.Unlock()
		if idle {
			if exitErr != nil {
				t.Fatal(exitErr)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle importer did not exit")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := store.Deliver(strings.NewReader("second")); err != nil {
		t.Fatal(err)
	}
	if store.Snapshot().Count() != 2 || store.Snapshot().LastID != 2 {
		t.Fatal("idle restart lost mailbox state")
	}
	file, err := store.Open(1)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil || string(data) != "first" {
		t.Fatalf("read after idle restart = %q, %v", data, err)
	}
}

func TestReadPipeDrainsEarlyCloseBeforeReusingReader(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := testNew(t, root)
	if err != nil {
		t.Fatal(err)
	}
	body := append([]byte("Subject: binary\r\n\r\n"), bytes.Repeat([]byte("\x00\ncheckpoint\nprogress fake\n"), 4000)...)
	if _, err := store.Deliver(bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	snapshot := store.Snapshot()
	file, err := store.OpenAt(snapshot, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	prefix := make([]byte, 17)
	if _, err := io.ReadFull(file, prefix); err != nil || !bytes.Equal(prefix, body[:17]) {
		t.Fatalf("pipe prefix: %q, %v", prefix, err)
	}
	done := make(chan error, 1)
	go func() { _, err := store.Deliver(strings.NewReader("second")); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("body reader blocked SMTP publication")
	}
	// The retained response is still pinned to the original commit, even after
	// a writer checkpoint, deletion and process shutdown occur independently.
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete([]uint64{1}); err != nil {
		t.Fatal(err)
	}

	again, err := store.OpenAt(snapshot, 1)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(again)
	closeErr := again.Close()
	if err != nil || closeErr != nil || !bytes.Equal(data, body) {
		t.Fatalf("binary pipe response changed: size %d, errors %v/%v", len(data), err, closeErr)
	}
	entries, err := os.ReadDir(filepath.Join(root, "tmp"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("unexpected temporary files: %v, %v", entries, err)
	}
}

func TestReadPipeEmptyBlobAndUnreadClose(t *testing.T) {
	t.Parallel()
	store, err := testNew(t, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"", "unread\x00body"} {
		if _, err := store.Deliver(strings.NewReader(body)); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []uint64{1, 2, 1} {
		file, err := store.Open(id)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	file, err := store.Open(2)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil || string(data) != "unread\x00body" {
		t.Fatalf("framing after unread close: %q, %v", data, err)
	}
}

func TestImporterCrashRequiresReopenAndKeepsAcceptedMail(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := testNew(t, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Deliver(strings.NewReader("accepted")); err != nil {
		t.Fatal(err)
	}
	store.gitMu.Lock()
	err = store.importer.cmd.Process.Kill()
	store.gitMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Deliver(strings.NewReader("unconfirmed")); err == nil {
		t.Fatal("accepted a write after importer died")
	}
	reopened, err := testNew(t, root)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Snapshot().Count() != 1 {
		t.Fatal("lost a confirmed commit or published incomplete input")
	}
	file, err := reopened.Open(1)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil || string(data) != "accepted" {
		t.Fatalf("accepted mail = %q, %v", data, err)
	}
}

func TestReaderCrashCanRestartWithoutPoisoningWriter(t *testing.T) {
	t.Parallel()
	store, err := testNew(t, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Deliver(strings.NewReader("first")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Lookup(store.Snapshot(), 1); err != nil {
		t.Fatal(err)
	}
	store.readMu.Lock()
	err = store.reader.cmd.Process.Kill()
	store.readMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Lookup(store.Snapshot(), 1); err == nil {
		t.Fatal("query succeeded after reader was killed")
	}
	if _, err := store.Deliver(strings.NewReader("second")); err != nil {
		t.Fatal(err)
	}
	message, err := store.Lookup(store.Snapshot(), 2)
	if err != nil || message.ID != 2 {
		t.Fatalf("reader restart: %+v, %v", message, err)
	}
}

func TestWaitingDeletionDoesNotBlockReceipt(t *testing.T) {
	t.Parallel()
	store, err := testNew(t, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Deliver(strings.NewReader("old")); err != nil {
		t.Fatal(err)
	}
	file, err := store.Open(1)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	deletion := make(chan error, 1)
	go func() { deletion <- store.Delete([]uint64{1}) }()
	receipt := make(chan error, 1)
	go func() { _, err := store.Deliver(strings.NewReader("new")); receipt <- err }()
	select {
	case err := <-receipt:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pending deletion held the state lock behind a slow reader")
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-deletion:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("deletion did not resume")
	}
	if snapshot := store.Snapshot(); snapshot.Count() != 1 || snapshot.LastID != 2 {
		t.Fatalf("concurrent receipt was overwritten: %+v", snapshot)
	}
}

func TestGitTimeoutStopsStalledProcesses(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"reader", "writer"} {
		t.Run(kind, func(t *testing.T) {
			store, err := testNew(t, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Deliver(strings.NewReader("accepted")); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Lookup(store.Snapshot(), 1); err != nil {
				t.Fatal(err)
			}
			if kind == "reader" {
				store.readMu.Lock()
				store.reader.timeout = 30 * time.Millisecond
				err = syscall.Kill(-store.reader.cmd.Process.Pid, syscall.SIGSTOP)
				store.readMu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.Lookup(store.Snapshot(), 1); !errors.Is(err, ErrGitTimeout) {
					t.Fatalf("stalled reader: %v", err)
				}
				if _, err := store.Deliver(strings.NewReader("next")); err != nil {
					t.Fatal(err)
				}
				if _, err := store.Lookup(store.Snapshot(), 2); err != nil {
					t.Fatalf("reader did not restart: %v", err)
				}
			} else {
				store.gitMu.Lock()
				store.importer.timeout = 30 * time.Millisecond
				err = syscall.Kill(-store.importer.cmd.Process.Pid, syscall.SIGSTOP)
				store.gitMu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.Deliver(strings.NewReader("uncertain")); !errors.Is(err, ErrGitTimeout) {
					t.Fatalf("stalled writer: %v", err)
				}
				if _, err := store.Deliver(strings.NewReader("blocked")); err == nil {
					t.Fatal("continued after uncertain publication")
				}
				reopened, err := testNew(t, store.root)
				if err != nil {
					t.Fatal(err)
				}
				if reopened.Snapshot().Count() != 1 {
					t.Fatal("timeout changed accepted mailbox")
				}
			}
		})
	}
}
