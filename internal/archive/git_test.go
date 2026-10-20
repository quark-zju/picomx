package archive

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestGitDeletionHistorySnapshotsAndMonotonicIDs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := testNew(t, root)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"first", "second", "third", "fourth", "fifth"} {
		if _, err := store.Deliver(strings.NewReader(body)); err != nil {
			t.Fatal(err)
		}
	}
	before := store.Snapshot()
	if err := store.Delete([]uint64{3, 2, 3}); err != nil {
		t.Fatal(err)
	}
	after := store.Snapshot()
	if after.Count() != 3 || after.TotalOctets != uint64(len("firstfourthfifth")) {
		t.Fatalf("snapshot after delete: %+v", after)
	}
	if !reflect.DeepEqual(store.state.Directories, directoryCounts{"1": 3}) {
		t.Fatalf("directories = %v", store.state.Directories)
	}

	for _, id := range []uint64{2, 3} {
		if _, err := store.Open(id); !errors.Is(err, ErrNoSuchMessage) {
			t.Fatalf("Open deleted %d: %v", id, err)
		}
		if _, err := os.Stat(filepath.Join(root, "1", []string{"002.eml", "003.eml"}[id-2])); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("deleted file remains: %v", err)
		}
	}
	content, err := store.OpenAt(before, 2)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(content)
	_ = content.Close()
	if err != nil || string(data) != "second" {
		t.Fatalf("historical read = %q, %v", data, err)
	}
	if size, err := store.SizeAt(before, 2); err != nil || size != 6 {
		t.Fatalf("historical size = %d, %v", size, err)
	}
	if _, err := store.OpenAt(after, 2); !errors.Is(err, ErrNoSuchMessage) {
		t.Fatalf("new snapshot accepted deleted ID: %v", err)
	}
	head := store.headID
	if err := store.Delete([]uint64{2, 3}); err != nil {
		t.Fatal(err)
	}
	if store.headID != head {
		t.Fatal("idempotent delete made another commit")
	}
	if err := store.Delete([]uint64{4}); err != nil {
		t.Fatal(err)
	}
	if after.index.total != 3 || after.index.leaves[0].count != 3 {
		t.Fatal("later deletion mutated an existing snapshot")
	}
	path, err := store.Deliver(strings.NewReader("sixth"))
	if err != nil {
		t.Fatal(err)
	}
	if path != "1/006-s5.eml" {
		t.Fatalf("reused an ID: %s", path)
	}
	reopened, err := testNew(t, root)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Snapshot().Count() != 3 || reopened.Snapshot().LastID != 6 {
		t.Fatalf("reopened snapshot: %+v", reopened.Snapshot())
	}
	for number, want := range []uint64{1, 5, 6} {
		if got, err := reopened.Lookup(reopened.Snapshot(), uint64(number+1)); err != nil || got.ID != want {
			t.Fatalf("ID(%d) = %d, %v", number+1, got.ID, err)
		}
	}
	if _, err := reopened.git(nil, "merge-base", "--is-ancestor", before.commit, "HEAD"); err != nil {
		t.Fatalf("original commit is no longer reachable: %v", err)
	}
	if _, err := reopened.git(nil, "fsck", "--strict"); err != nil {
		t.Fatalf("invalid repository: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "index")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bare repo retained an index: %v", err)
	}
}

func TestDeleteValidationAndAllDeletedRestart(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := testNew(t, root)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := store.Deliver(strings.NewReader("mail")); err != nil {
			t.Fatal(err)
		}
	}
	head := store.headID
	if err := store.Delete([]uint64{1, 4}); !errors.Is(err, ErrNoSuchMessage) {
		t.Fatalf("invalid batch: %v", err)
	}
	if store.headID != head || store.Snapshot().Count() != 3 {
		t.Fatal("invalid batch partially deleted messages")
	}
	if err := store.Delete([]uint64{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	reopened, err := testNew(t, root)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Snapshot().Count() != 0 || reopened.Snapshot().TotalOctets != 0 {
		t.Fatalf("all-deleted mailbox: %+v", reopened.Snapshot())
	}
	path, err := reopened.Deliver(strings.NewReader("new"))
	if err != nil {
		t.Fatal(err)
	}
	if path != "1/004-s3.eml" {
		t.Fatalf("reused deleted ID: %s", path)
	}
}

func TestFailedRefUpdateLeavesOldMailboxAndRecovers(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := testNew(t, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Deliver(strings.NewReader("original")); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(root, "refs", "heads", "main.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete([]uint64{1}); err == nil {
		t.Fatal("deletion succeeded despite ref lock")
	}
	if raw, err := store.git(nil, "show", "HEAD:1/001-s8.eml"); err != nil || string(raw) != "original" {
		t.Fatalf("failed deletion removed original: %q, %v", raw, err)
	}
	if _, err := store.Deliver(strings.NewReader("next")); err == nil {
		t.Fatal("store continued writing after an unresolved transaction")
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	reopened, err := testNew(t, root)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Snapshot().Count() != 1 {
		t.Fatalf("failed deletion persisted: %+v", reopened.Snapshot())
	}
	if _, err := reopened.Deliver(strings.NewReader("next")); err != nil {
		t.Fatal(err)
	}
}

func TestGitIgnoresHooksFiltersAndInheritedEnvironment(t *testing.T) {
	// t.Setenv intentionally makes this test serial.
	root := t.TempDir()
	store, err := testNew(t, root)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := "#!/bin/sh\ntouch '" + marker + "'\nexit 1\n"
	// Empty templates produce no hooks directory; create it explicitly.
	if err := os.MkdirAll(filepath.Join(root, "hooks"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"reference-transaction", "post-index-change", "pre-commit"} {
		if err := os.WriteFile(filepath.Join(root, "hooks", name), []byte(hook), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.git(nil, "config", "core.fsmonitor", filepath.Join(root, "hooks", "pre-commit")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.git(nil, "config", "commit.gpgSign", "true"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.git(nil, "config", "core.autocrlf", "true"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_DIR", t.TempDir())
	t.Setenv("GIT_INDEX_FILE", filepath.Join(t.TempDir(), "unrelated-index"))
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", filepath.Join(root, "hooks"))
	message := "Subject: raw\r\n\r\nbody\r\n"
	if _, err := store.Deliver(strings.NewReader(message)); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete([]uint64{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Git executed a hook: %v", err)
	}
	raw, err := store.git(nil, "show", "HEAD~1:1/001-s22.eml")
	if err != nil || string(raw) != message {
		t.Fatalf("Git changed raw message bytes: %q, %v", raw, err)
	}
}

func TestBareImportPreservesOriginalsAndUsesCommittedStateOnRestart(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "1"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "1", "001.eml")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy := []byte("{\"version\":1,\"last_id\":1,\"total_octets\":8}\n")
	if err := os.WriteFile(filepath.Join(root, "state"), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := testNew(t, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete([]uint64{1}); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(path); err != nil || string(content) != "original" {
		t.Fatalf("migration changed original: %q, %v", content, err)
	}
	reopened, err := testNew(t, root)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Snapshot().Count() != 0 {
		t.Fatal("stale legacy state resurrected deleted mail")
	}
	if _, err := reopened.Deliver(strings.NewReader("new")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "1", "002.eml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created working copy: %v", err)
	}
}

func TestBareImportRejectsSymlinkMessage(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "target"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "target"), filepath.Join(root, "1", "001.eml")); err != nil {
		t.Fatal(err)
	}
	if err := writeMailboxState(root, mailboxState{Version: mailboxStateVersion, LastID: 1, TotalOctets: 8}); err != nil {
		t.Fatal(err)
	}
	if _, err := testNew(t, root); err == nil {
		t.Fatal("import accepted a symlink message")
	}
}

func TestBareRejectsExistingWorkingRepository(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := testNew(t, root); err == nil || !strings.Contains(err.Error(), "git clone --bare") {
		t.Fatalf("working repository rejection: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "HEAD")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("modified a working repository")
	}
}

func TestLostHEADDoesNotReimportLegacyMailOrReuseIDs(t *testing.T) {
	t.Parallel()
	for _, missing := range []string{"HEAD", "branch"} {
		t.Run(missing, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "1"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "1", "001.eml"), []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := writeMailboxState(root, mailboxState{Version: mailboxStateVersion, LastID: 1, TotalOctets: 8}); err != nil {
				t.Fatal(err)
			}
			store, err := testNew(t, root)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Delete([]uint64{1}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Deliver(strings.NewReader("second")); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			if missing == "HEAD" {
				if err := os.Remove(filepath.Join(root, "HEAD")); err != nil {
					t.Fatal(err)
				}
			} else if _, err := store.git(nil, "update-ref", "-d", "refs/heads/main"); err != nil {
				t.Fatal(err)
			}
			if _, err := testNew(t, root); err == nil || !strings.Contains(err.Error(), "restore HEAD") {
				t.Fatalf("reinitialized existing archive: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "HEAD")); missing == "HEAD" && !errors.Is(err, os.ErrNotExist) {
				t.Fatal("recreated HEAD before recovery")
			}
		})
	}
}

func TestDeletionCommitRecordsActorWithoutForgedMetadataLines(t *testing.T) {
	t.Parallel()
	store, err := testNew(t, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, username := range []string{"alice", "用户", "line\nbreak"} {
		if _, err := store.Deliver(strings.NewReader("body")); err != nil {
			t.Fatal(err)
		}
		id := store.Snapshot().LastID
		if err := store.DeleteAs([]uint64{id}, username); err != nil {
			t.Fatal(err)
		}
		message, err := store.git(nil, "log", "-1", "--format=%B")
		if err != nil {
			t.Fatal(err)
		}
		actor := username
		if username == "line\nbreak" {
			actor = `"line\nbreak"`
		}
		want := "mail: delete 1 messages\n\nPOP3 user: " + actor + "\n"
		if string(message) != want {
			t.Fatalf("commit message = %q, want %q", message, want)
		}
		head := store.headID
		if err := store.DeleteAs([]uint64{id}, "another-user"); err != nil {
			t.Fatal(err)
		}
		if store.headID != head {
			t.Fatal("idempotent delete generated a misleading actor commit")
		}
	}
}
