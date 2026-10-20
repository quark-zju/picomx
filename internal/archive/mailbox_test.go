package archive

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMailboxSnapshotOpenAndSize(t *testing.T) {
	t.Parallel()

	store, err := testNew(t, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Snapshot(); got.Count() != 0 || got.TotalOctets != 0 {
		t.Fatalf("empty snapshot = %+v", got)
	}
	message := "Header: value\r\n\r\nbody\r\n"
	if _, err := store.Deliver(strings.NewReader(message)); err != nil {
		t.Fatal(err)
	}
	if got, want := store.Snapshot(), (Snapshot{LastID: 1, TotalOctets: uint64(len(message))}); got.LastID != want.LastID || got.Count() != want.LastID || got.TotalOctets != want.TotalOctets {
		t.Fatalf("snapshot = %+v, want %+v", got, want)
	}
	if got, err := store.Size(1); err != nil || got != uint64(len(message)) {
		t.Fatalf("Size(1) = %d, %v", got, err)
	}
	file, err := store.Open(1)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(content); got != message {
		t.Fatalf("Open(1) content = %q, want %q", got, message)
	}
}

func TestMailboxRejectsIDsOutsideSnapshot(t *testing.T) {
	t.Parallel()

	store, err := testNew(t, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []uint64{0, 1} {
		if _, err := store.Open(id); !errors.Is(err, ErrNoSuchMessage) {
			t.Fatalf("Open(%d) error = %v, want ErrNoSuchMessage", id, err)
		}
		if _, err := store.Size(id); !errors.Is(err, ErrNoSuchMessage) {
			t.Fatalf("Size(%d) error = %v, want ErrNoSuchMessage", id, err)
		}
	}
}

func TestMailboxReadsCommittedBlobInsteadOfUntrackedFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := testNew(t, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Deliver(strings.NewReader("message")); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "1", "001.eml")); err != nil {
		t.Fatal(err)
	}
	file, err := store.Open(1)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil || string(content) != "message" {
		t.Fatalf("committed content = %q, %v", content, err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if size, err := store.Size(1); err != nil || size != 7 {
		t.Fatalf("committed size = %d, %v", size, err)
	}
}
