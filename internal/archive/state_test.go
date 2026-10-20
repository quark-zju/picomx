package archive

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestReadMailboxStateTreatsMissingFileAsEmpty(t *testing.T) {
	t.Parallel()

	state, err := readMailboxState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(state, emptyMailboxState()) {
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
	if !reflect.DeepEqual(got, want) {
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
		"wrong version":  `{"version":3,"last_id":0,"total_octets":0}`,
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

func TestReadLegacyStateUpgradesWithoutReassigningIDs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "state"), []byte(`{"version":1,"last_id":123,"total_octets":456}`), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := readMailboxState(root)
	if err != nil {
		t.Fatal(err)
	}
	if state.Version != mailboxStateVersion || state.LastID != 123 || state.TotalOctets != 456 || len(state.Directories) != 0 {
		t.Fatalf("legacy state = %+v", state)
	}
}

func TestDecodeGitStateUsesDirectoryCounts(t *testing.T) {
	t.Parallel()
	state, err := decodeMailboxState(strings.NewReader(`{"version":2,"last_id":1026,"total_octets":7,"directories":{"1":2,"2":1,"2/001":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	if state.index.total != 3 || state.index.leaves[1].accumulated != 3 {
		t.Fatalf("index = %+v", state.index)
	}
	for _, content := range []string{
		`{"version":1,"last_id":0,"total_octets":0}`,
		`{"version":2,"last_id":1,"total_octets":1,"deleted_ranges":[]}`,
		`{"version":2,"last_id":1,"total_octets":1}`,
		`{"version":2,"last_id":1024,"total_octets":1,"directories":{"2/001":1}}`,
		`{"version":2,"last_id":0,"total_octets":0} {}`,
	} {
		if _, err := decodeMailboxState(strings.NewReader(content)); err == nil {
			t.Fatalf("accepted %s", content)
		}
	}
}
