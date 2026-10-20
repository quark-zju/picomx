package archive

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	store.gitMu.Unlock()
	if _, err := store.Deliver(strings.NewReader("first")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		store.gitMu.Lock()
		idle := store.importer == nil && store.reader == nil
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
	file, err := store.Open(1)
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
		t.Fatalf("write bypassed the leased Git pipe: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("closing the Git reader did not release the writer")
	}
	again, err := store.Open(1)
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
	store.gitMu.Lock()
	err = store.reader.cmd.Process.Kill()
	store.gitMu.Unlock()
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
