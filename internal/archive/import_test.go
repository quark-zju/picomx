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
		idle := store.importer == nil
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

func TestReadSpoolReleasesImporterBeforeClientConsumesMessage(t *testing.T) {
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
	done := make(chan error, 1)
	go func() { _, err := store.Deliver(strings.NewReader("second")); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("unconsumed POP body blocked the Git writer")
	}
	data, err := io.ReadAll(file)
	if err != nil || !bytes.Equal(data, body) {
		t.Fatalf("binary message changed: length %d, error %v", len(data), err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "tmp"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary files leaked: %v, %v", entries, err)
	}
	if _, err := os.Stat(filepath.Join(root, "index")); !os.IsNotExist(err) {
		t.Fatal("importer created an index")
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
