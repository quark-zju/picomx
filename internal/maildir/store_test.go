package maildir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDeliverWritesMessageAtomicallyToMonthlyMaildir(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	now := time.Date(2026, time.August, 30, 12, 34, 56, 789, time.FixedZone("PDT", -7*60*60))
	store := newStore(root, "mail/host", func() time.Time { return now })

	path, err := store.Deliver(strings.NewReader("From: sender@example.org\r\n\r\nhello\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := filepath.Dir(path), filepath.Join(root, "2026", "08", "new"); got != want {
		t.Fatalf("message directory = %q, want %q", got, want)
	}
	if !strings.HasSuffix(path, ".mail_host") {
		t.Fatalf("unsafe hostname was not sanitized in %q", path)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(content), "From: sender@example.org\r\n\r\nhello\r\n"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Fatalf("mode = %o, want %o", got, want)
	}
	entries, err := os.ReadDir(filepath.Join(root, "2026", "08", "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("tmp contains %d entries after delivery", len(entries))
	}
}

func TestDeliverUsesUniqueNamesAtSameInstant(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	now := time.Unix(123, 456)
	store := newStore(root, "host", func() time.Time { return now })

	first, err := store.Deliver(strings.NewReader("first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Deliver(strings.NewReader("second"))
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("deliveries reused path %q", first)
	}
}

func TestNewRejectsEmptyRoot(t *testing.T) {
	t.Parallel()

	if _, err := New("  "); err == nil {
		t.Fatal("New accepted an empty root")
	}
}
