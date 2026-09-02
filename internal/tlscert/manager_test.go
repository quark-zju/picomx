package tlscert

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestManagerReloadsChangedCertificate(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	directory := filepath.Join(root, "example.com")
	writeTestCertificate(t, directory, []string{"*.example.com"})
	manager, err := New(root, []string{"mx.example.com", "pop.example.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	before, err := manager.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	beforeDER := append([]byte(nil), before.Certificate[0]...)

	writeTestCertificate(t, directory, []string{"*.example.com"})
	changed, err := manager.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("Reload did not report changed certificate content")
	}
	after, err := manager.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(beforeDER, after.Certificate[0]) {
		t.Fatal("certificate bytes did not change")
	}
}

func TestExpirationLevel(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000, 0)
	tests := []struct {
		remaining time.Duration
		want      string
	}{
		{remaining: -time.Second, want: "expired"},
		{remaining: time.Hour, want: "1d"},
		{remaining: 3 * 24 * time.Hour, want: "7d"},
		{remaining: 20 * 24 * time.Hour, want: "30d"},
		{remaining: 31 * 24 * time.Hour, want: ""},
	}
	for _, test := range tests {
		if got := expirationLevel(now.Add(test.remaining), now); got != test.want {
			t.Errorf("expirationLevel(%v) = %q, want %q", test.remaining, got, test.want)
		}
	}
}

func TestManagerRejectsExpiredReplacement(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	directory := filepath.Join(root, "example.com")
	writeTestCertificate(t, directory, []string{"*.example.com"})
	manager, err := New(root, []string{"mx.example.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	before, err := manager.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	writeTestCertificateAt(t, directory, []string{"*.example.com"}, now.Add(-2*time.Hour), now.Add(-time.Hour))
	if changed, err := manager.Reload(); err == nil || changed {
		t.Fatalf("Reload() = %v, %v, want unchanged expiration error", changed, err)
	}
	after, err := manager.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("expired replacement replaced current certificate")
	}
}

func TestManagerKeepsCertificateWhenReloadFails(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	directory := filepath.Join(root, "example.com")
	writeTestCertificate(t, directory, []string{"*.example.com"})
	manager, err := New(root, []string{"mx.example.com"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	before, err := manager.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, privateKeyName), []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, err := manager.Reload(); err == nil || changed {
		t.Fatalf("Reload() = %v, %v, want unchanged error", changed, err)
	}
	after, err := manager.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("failed reload replaced current certificate")
	}
}
