package archive

import (
	"math"
	"path/filepath"
	"testing"
)

func TestMessageRelativePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		id   uint64
		want string
	}{
		{id: 1, want: filepath.Join("1", "001.eml")},
		{id: 1023, want: filepath.Join("1", "3ff.eml")},
		{id: 1024, want: filepath.Join("2", "001", "000.eml")},
		{id: 1025, want: filepath.Join("2", "001", "001.eml")},
		{id: 1024*1024 - 1, want: filepath.Join("2", "3ff", "3ff.eml")},
		{id: 1024 * 1024, want: filepath.Join("3", "001", "000", "000.eml")},
		{id: math.MaxUint64, want: filepath.Join("7", "00f", "3ff", "3ff", "3ff", "3ff", "3ff", "3ff.eml")},
	}

	for _, test := range tests {
		got, err := messageRelativePath(test.id)
		if err != nil {
			t.Fatalf("messageRelativePath(%d): %v", test.id, err)
		}
		if got != test.want {
			t.Errorf("messageRelativePath(%d) = %q, want %q", test.id, got, test.want)
		}
	}
}

func TestMessageRelativePathRejectsZero(t *testing.T) {
	t.Parallel()

	if _, err := messageRelativePath(0); err == nil {
		t.Fatal("messageRelativePath(0) succeeded")
	}
}
