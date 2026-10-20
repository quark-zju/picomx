package archive

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTreeParserHandlesBothHashFormatsAndRejectsBadEntries(t *testing.T) {
	t.Parallel()
	for _, hashBytes := range []int{20, 32} {
		entry := append([]byte("100644 001-s3.eml\x00"), bytes.Repeat([]byte{0x42}, hashBytes)...)
		got, err := parseTree(entry, hashBytes)
		if err != nil || len(got) != 1 || got[0].oid != strings.Repeat("42", hashBytes) {
			t.Fatalf("parse SHA-%d: %+v, %v", hashBytes*8, got, err)
		}
		for _, bad := range [][]byte{entry[:len(entry)-1], append(append([]byte{}, entry...), entry...), append([]byte("120000 link\x00"), bytes.Repeat([]byte{0}, hashBytes)...), []byte("100644 no-terminator")} {
			if _, err := parseTree(bad, hashBytes); err == nil {
				t.Fatalf("accepted malformed tree %q", bad)
			}
		}
	}
}

// Build a whole contiguous fixture in one transaction, including fanout
// boundaries, without spending a durable Git checkpoint on every test message.
func fixtureMessages(t *testing.T, store *Store, count uint64) {
	t.Helper()
	state := emptyMailboxState()
	state.LastID = count
	state.Directories = directoryCounts{}
	changes := map[string][]byte{}
	for id := uint64(1); id <= count; id++ {
		body := []byte(fmt.Sprintf("message %d", id))
		path, _ := messagePath(id, uint64(len(body)))
		changes[path] = body
		state.TotalOctets += uint64(len(body))
		state.Directories.add(filepath.ToSlash(filepath.Dir(path)))
	}
	var err error
	state.index, err = buildDirectoryIndex(state.Directories, state.LastID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.commitChanges("mail: test fixture", state, changes, nil); err != nil {
		t.Fatal(err)
	}
	store.state = state
}

func TestDirectoryCountsAcrossFanoutAndTreeCacheReuse(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := testNew(t, root)
	if err != nil {
		t.Fatal(err)
	}
	fixtureMessages(t, store, 2050)
	before := store.Snapshot()
	message, err := store.Lookup(before, 1024)
	if err != nil || message.ID != 1024 {
		t.Fatalf("lookup at fanout boundary: %+v, %v", message, err)
	}
	header, err := store.objectInfo(before.commit+":2/001", "tree")
	if err != nil {
		t.Fatal(err)
	}
	key := directoryKey{"2/001", header.oid}
	cached, ok := store.cache.get(key)
	if !ok {
		t.Fatal("leaf listing was not cached")
	}
	if err := store.Delete([]uint64{1, 2, 3, 2048, 2049, 2050, 2050}); err != nil {
		t.Fatal(err)
	}
	after := store.Snapshot()
	want := directoryCounts{"1": 1020, "2": 1024, "2/001": 1024}
	if !reflect.DeepEqual(store.state.Directories, want) {
		t.Fatalf("counts = %v, want %v", store.state.Directories, want)
	}
	message, err = store.Lookup(after, 1021)
	if err != nil || message.ID != 1024 {
		t.Fatalf("rank after delete: %+v, %v", message, err)
	}
	reused, ok := store.cache.get(key)
	if !ok || &reused[0] != &cached[0] {
		t.Fatal("unchanged directory was not reused across commits")
	}
	if before.Count() != 2050 || before.index.leaves[0].count != 1023 {
		t.Fatal("batch deletion mutated old snapshot")
	}
	var ids []uint64
	if err := store.ListAt(after, func(number, id, size uint64) error { ids = append(ids, id); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2044 || ids[0] != 4 || ids[len(ids)-1] != 2047 {
		t.Fatalf("listing after directory removal: %d entries", len(ids))
	}
	reopened, err := testNew(t, root)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Snapshot().Count() != 2044 {
		t.Fatal("directory counts lost on reopen")
	}
}

func TestBareSHA256TreeAndBodyRead(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	initializer := &Store{root: root}
	if _, err := initializer.git(nil, "init", "--bare", "--template=", "--initial-branch=main", "--object-format=sha256"); err != nil {
		t.Fatal(err)
	}
	store, err := testNew(t, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Deliver(strings.NewReader("sha256\x00body")); err != nil {
		t.Fatal(err)
	}
	file, err := store.Open(1)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(file)
	if err := errors.Join(readErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	if string(data) != "sha256\x00body" {
		t.Fatalf("body = %q", data)
	}
	if _, err := testNew(t, root); err != nil {
		t.Fatal(err)
	}
}

func TestMessageReadChecksFilenameSizeWithoutStartupScan(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := testNew(t, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Deliver(strings.NewReader("original")); err != nil {
		t.Fatal(err)
	}
	if err := store.commitChanges("mail: inconsistent fixture", store.state, map[string][]byte{"1/001-s8.eml": []byte("wrong length")}, nil); err != nil {
		t.Fatal(err)
	}
	reopened, err := testNew(t, root)
	if err != nil {
		t.Fatalf("startup scanned blobs: %v", err)
	}
	if len(reopened.cache.entries) != 0 {
		t.Fatal("startup enumerated mail directories")
	}
	if _, err := reopened.Open(1); err == nil {
		t.Fatal("body read ignored the framed size mismatch")
	}
}
