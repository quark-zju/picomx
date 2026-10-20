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
	// Reproduce the legacy crash window before importing the archive into Git.
	if err := os.MkdirAll(filepath.Join(root, "1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "1", "001.eml"), []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeMailboxState(root, emptyMailboxState()); err != nil {
		t.Fatal(err)
	}

	recovered, err := testNew(t, root)
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
	if got, want := path, "1/002-s6.eml"; got != want {
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
	if _, err := testNew(t, root); err == nil {
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
	if _, err := testNew(t, root); err == nil {
		t.Fatal("New accepted a missing indexed tail message")
	}
}
