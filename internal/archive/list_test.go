package archive

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestListAtReusesReaderAndPinsSnapshot(t *testing.T) {
	// A PATH wrapper measures process count; t.Setenv makes this test serial.
	root := t.TempDir()
	store, err := testNew(t, root)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"a", "bb", "ccc", "dddd"} {
		if _, err := store.Deliver(strings.NewReader(body)); err != nil {
			t.Fatal(err)
		}
	}
	before := store.Snapshot()
	if err := store.Delete([]uint64{2}); err != nil {
		t.Fatal(err)
	}
	after := store.Snapshot()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	log := filepath.Join(bin, "invocations")
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + quote(log) + "\nexec " + quote(realGit) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	type row struct{ number, id, size uint64 }
	for _, test := range []struct {
		snapshot Snapshot
		want     []row
	}{
		{before, []row{{1, 1, 1}, {2, 2, 2}, {3, 3, 3}, {4, 4, 4}}},
		{after, []row{{1, 1, 1}, {2, 3, 3}, {3, 4, 4}}},
	} {
		if err := os.WriteFile(log, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		var got []row
		if err := store.ListAt(test.snapshot, func(number, id, size uint64) error {
			got = append(got, row{number, id, size})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, test.want) {
			t.Fatalf("listing = %v, want %v", got, test.want)
		}
		commands, err := os.ReadFile(log)
		if err != nil {
			t.Fatal(err)
		}
		if len(commands) != 0 {
			t.Fatalf("listing spawned a child instead of reusing cat-file: %q", commands)
		}
	}
	// After idle shutdown, a directory miss restarts just one reader, and
	// subsequent LIST/UIDL operations reuse it even when switching snapshots.
	store.readMu.Lock()
	err = store.closeReaderLocked()
	store.readMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(log, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	store.cache = newDirectoryCache(directoryCacheCapacity)
	for _, snapshot := range []Snapshot{before, after, before} {
		if err := store.ListAt(snapshot, func(number, id, size uint64) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	commands, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(commands)), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "cat-file --batch-command") {
		t.Fatalf("reader restart spawned unexpected children: %q", commands)
	}
	stop := errors.New("stop listing")
	if err := store.ListAt(before, func(number, id, size uint64) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("callback error = %v", err)
	}
}
