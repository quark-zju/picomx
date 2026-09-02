package archive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewRecoversOnePublishedTailMessage(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Deliver(strings.NewReader("first")); err != nil {
		t.Fatal(err)
	}
	if err := writeMailboxState(root, emptyMailboxState()); err != nil {
		t.Fatal(err)
	}

	recovered, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	state, err := readMailboxState(root)
	if err != nil {
		t.Fatal(err)
	}
	if state.LastID != 1 || state.TotalOctets != uint64(len("first")) {
		t.Fatalf("recovered state = %+v", state)
	}
	path, err := recovered.Deliver(strings.NewReader("second"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := path, filepath.Join(root, "1", "002.eml"); got != want {
		t.Fatalf("next path = %q, want %q", got, want)
	}
}

func TestNewRejectsStateThatTrailsMultipleMessages(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	for id, content := range map[uint64]string{1: "first", 2: "second"} {
		relativePath, err := messageRelativePath(id)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, relativePath)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := New(root); err == nil {
		t.Fatal("New accepted state trailing multiple messages")
	}
}

func TestNewRejectsMissingIndexedTail(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	state := mailboxState{Version: mailboxStateVersion, LastID: 1, TotalOctets: 5}
	if err := writeMailboxState(root, state); err != nil {
		t.Fatal(err)
	}
	if _, err := New(root); err == nil {
		t.Fatal("New accepted a missing indexed tail message")
	}
}
