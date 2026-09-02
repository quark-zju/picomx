package archive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeliverPublishesSequentialMessageAndState(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	message := "From: sender@example.org\r\n\r\nhello\r\n"
	path, err := store.Deliver(strings.NewReader(message))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := path, filepath.Join(root, "1", "001.eml"); got != want {
		t.Fatalf("message path = %q, want %q", got, want)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(content); got != message {
		t.Fatalf("message = %q, want %q", got, message)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Fatalf("mode = %o, want %o", got, want)
	}
	state, err := readMailboxState(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := state.LastID, uint64(1); got != want {
		t.Fatalf("last ID = %d, want %d", got, want)
	}
	if got, want := state.TotalOctets, uint64(len(message)); got != want {
		t.Fatalf("total octets = %d, want %d", got, want)
	}
	entries, err := os.ReadDir(filepath.Join(root, "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("tmp contains %d entries after delivery", len(entries))
	}
}

func TestDeliverUsesSequentialIDs(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Deliver(strings.NewReader("first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Deliver(strings.NewReader("second"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := first, filepath.Join(root, "1", "001.eml"); got != want {
		t.Fatalf("first path = %q, want %q", got, want)
	}
	if got, want := second, filepath.Join(root, "1", "002.eml"); got != want {
		t.Fatalf("second path = %q, want %q", got, want)
	}
	state, err := readMailboxState(root)
	if err != nil {
		t.Fatal(err)
	}
	if state.LastID != 2 || state.TotalOctets != uint64(len("firstsecond")) {
		t.Fatalf("state = %+v", state)
	}
}

func TestStageRemainsInvisibleUntilPublish(t *testing.T) {
	t.Parallel()

	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	staged, err := store.Stage(strings.NewReader("staged"))
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Discard()
	if got := store.Snapshot(); got != (Snapshot{}) {
		t.Fatalf("snapshot before publish = %+v", got)
	}
	file, err := staged.Open()
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(file.Name())
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "staged" || staged.Size() != uint64(len("staged")) {
		t.Fatalf("staged content = %q, size %d", content, staged.Size())
	}
	id, path, err := store.Publish(staged)
	if err != nil {
		t.Fatal(err)
	}
	if id != 1 || path == "" {
		t.Fatalf("Publish = %d, %q", id, path)
	}
	if got := store.Snapshot(); got != (Snapshot{LastID: 1, TotalOctets: 6}) {
		t.Fatalf("snapshot after publish = %+v", got)
	}
}

func TestDeliverNeverOverwritesExistingMessage(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	firstStore, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	secondStore, err := New(root)
	if err != nil {
		t.Fatal(err)
	}

	path, err := firstStore.Deliver(strings.NewReader("original"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secondStore.Deliver(strings.NewReader("replacement")); err == nil {
		t.Fatal("colliding delivery overwrote an existing archive path")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(content), "original"; got != want {
		t.Fatalf("existing message = %q, want %q", got, want)
	}
}

func TestNewRejectsEmptyRoot(t *testing.T) {
	t.Parallel()

	if _, err := New("  "); err == nil {
		t.Fatal("New accepted an empty root")
	}
}
