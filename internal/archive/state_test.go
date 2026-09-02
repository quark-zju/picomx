package archive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadMailboxStateTreatsMissingFileAsEmpty(t *testing.T) {
	t.Parallel()

	state, err := readMailboxState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if state != emptyMailboxState() {
		t.Fatalf("state = %+v, want %+v", state, emptyMailboxState())
	}
}

func TestWriteMailboxStateRoundTrips(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	want := mailboxState{Version: mailboxStateVersion, LastID: 42, TotalOctets: 12345}
	if err := writeMailboxState(root, want); err != nil {
		t.Fatal(err)
	}
	got, err := readMailboxState(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("state = %+v, want %+v", got, want)
	}

	info, err := os.Stat(filepath.Join(root, mailboxStateName))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Fatalf("state mode = %o, want %o", got, want)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != mailboxStateName {
		t.Fatalf("archive root entries = %v, want only state", entries)
	}
}

func TestReadMailboxStateRejectsInvalidContent(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"wrong version":  `{"version":2,"last_id":0,"total_octets":0}`,
		"unknown field":  `{"version":1,"last_id":0,"total_octets":0,"extra":1}`,
		"trailing value": `{"version":1,"last_id":0,"total_octets":0} {}`,
		"invalid empty":  `{"version":1,"last_id":0,"total_octets":1}`,
	}
	for name, content := range tests {
		name, content := name, content
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, mailboxStateName), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readMailboxState(root); err == nil {
				t.Fatal("readMailboxState accepted invalid content")
			} else if strings.TrimSpace(err.Error()) == "" {
				t.Fatal("readMailboxState returned an empty error")
			}
		})
	}
}
