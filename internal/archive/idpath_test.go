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

func TestSizedMessagePathsRoundTrip(t *testing.T) {
	t.Parallel()
	for _, id := range []uint64{1, 1023, 1024, 1025, 1024 * 1024, math.MaxUint64} {
		for _, size := range []uint64{0, 123, math.MaxUint64} {
			path, err := messagePath(id, size)
			if err != nil {
				t.Fatal(err)
			}
			got, err := messageFromPath(path)
			if err != nil || got.ID != id || got.Size != size || got.Path != path {
				t.Fatalf("round trip %q = %+v, %v", path, got, err)
			}
		}
	}
	for _, path := range []string{"1/001.eml", "1/001-s01.eml", "1/001-s-1.eml", "1/001-s18446744073709551616.eml", "1/000-s1.eml", "2/000/001-s1.eml", "1/001-s1", "../001-s1.eml", "1/001-s1.eml/x"} {
		if _, err := messageFromPath(path); err == nil {
			t.Fatalf("accepted %q", path)
		}
	}
}
