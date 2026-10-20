package archive

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeliverPublishesSequentialMessageAndState(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := testNew(t, root)
	if err != nil {
		t.Fatal(err)
	}
	message := "From: sender@example.org\r\n\r\nhello\r\n"
	path, err := store.Deliver(strings.NewReader(message))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := path, "1/001-s35.eml"; got != want {
		t.Fatalf("message path = %q, want %q", got, want)
	}
	content, err := store.git(nil, "cat-file", "blob", "HEAD:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(content); got != message {
		t.Fatalf("message = %q, want %q", got, message)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(path))); !os.IsNotExist(err) {
		t.Fatalf("bare archive created a working file: %v", err)
	}
	if bare, err := store.git(nil, "rev-parse", "--is-bare-repository"); err != nil || strings.TrimSpace(string(bare)) != "true" {
		t.Fatalf("not a bare repository: %s, %v", bare, err)
	}
	state := store.state
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
	store, err := testNew(t, root)
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
	if got, want := first, "1/001-s5.eml"; got != want {
		t.Fatalf("first path = %q, want %q", got, want)
	}
	if got, want := second, "1/002-s6.eml"; got != want {
		t.Fatalf("second path = %q, want %q", got, want)
	}
	state := store.state
	if state.LastID != 2 || state.TotalOctets != uint64(len("firstsecond")) {
		t.Fatalf("state = %+v", state)
	}
}

func TestStageRemainsInvisibleUntilPublish(t *testing.T) {
	t.Parallel()

	store, err := testNew(t, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	staged, err := store.Stage(strings.NewReader("staged"))
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Discard()
	info, err := os.Stat(staged.(*StagedMessage).path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("staging permissions: %v, %v", info, err)
	}
	if got := store.Snapshot(); got.Count() != 0 || got.TotalOctets != 0 {
		t.Fatalf("snapshot before publish = %+v", got)
	}
	file, err := staged.Open()
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(file)
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
	if got := store.Snapshot(); got.LastID != 1 || got.Count() != 1 || got.TotalOctets != 6 {
		t.Fatalf("snapshot after publish = %+v", got)
	}
}

func TestDeliverNeverOverwritesExistingMessage(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	firstStore, err := testNew(t, root)
	if err != nil {
		t.Fatal(err)
	}
	secondStore, err := testNew(t, root)
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
	content, err := firstStore.git(nil, "cat-file", "blob", "HEAD:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(content), "original"; got != want {
		t.Fatalf("existing message = %q, want %q", got, want)
	}
}

func TestNewRejectsEmptyRoot(t *testing.T) {
	t.Parallel()

	if _, err := testNew(t, "  "); err == nil {
		t.Fatal("New accepted an empty root")
	}
}

// testNew ensures children and timers stop before testing removes the repo.
func testNew(t *testing.T, root string) (*Store, error) {
	t.Helper()
	store, err := New(root)
	if err == nil {
		t.Cleanup(func() { _ = store.Close() })
	}
	return store, err
}
