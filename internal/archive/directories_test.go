package archive

import (
	"math"
	"math/rand"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestDirectoryRankMatchesSparseMailbox(t *testing.T) {
	t.Parallel()
	random := rand.New(rand.NewSource(39))
	ids := map[uint64]bool{1: true, 1023: true, 1024: true, 1025: true, 1024*1024 - 1: true, 1024 * 1024: true, math.MaxUint64: true}
	for range 3000 {
		ids[uint64(random.Int63())+1] = true
	}
	counts := directoryCounts{}
	leaves := map[string][]uint64{}
	for id := range ids {
		path, _ := messageRelativePath(id)
		leaf := filepath.ToSlash(filepath.Dir(path))
		counts.add(leaf)
		leaves[leaf] = append(leaves[leaf], id)
	}
	index, err := buildDirectoryIndex(counts, math.MaxUint64)
	if err != nil {
		t.Fatal(err)
	}
	ordered := make([]uint64, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	for _, values := range leaves {
		sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	}
	for number, want := range ordered {
		leaf, offset, ok := index.selectLeaf(uint64(number + 1))
		if !ok || leaves[leaf.path][offset] != want {
			t.Fatalf("rank %d: %v/%d, want ID %d", number+1, leaf, offset, want)
		}
	}
	for _, number := range []uint64{0, uint64(len(ids) + 1), math.MaxUint64} {
		if _, _, ok := index.selectLeaf(number); ok {
			t.Fatalf("accepted invalid rank %d", number)
		}
	}
	clone := cloneDirectoryCounts(counts)
	for id := range ids {
		path, _ := messageRelativePath(id)
		if err := clone.remove(filepath.ToSlash(filepath.Dir(path))); err != nil {
			t.Fatal(err)
		}
	}
	if len(clone) != 0 || len(counts) == 0 || index.total != uint64(len(ids)) {
		t.Fatal("deletion did not remove empty ancestors, or mutated old index")
	}
	if err := clone.remove("1"); err == nil {
		t.Fatal("accepted count underflow")
	}
}

func TestDirectoryIndexRejectsInvalidMetadata(t *testing.T) {
	t.Parallel()
	for _, counts := range []directoryCounts{
		{"1": 0}, {"1": 1024}, {"1": 1025}, {"2/001": 1}, {"2": 2, "2/001": 1}, {"2": 1},
		{"01": 1}, {"8": 1}, {"2": 1, "2/000": 1}, {"2": 1, "2/400": 1},
		{"2": 1, "2/001/002": 1}, {"1": 1, "../x": 1},
	} {
		if _, err := buildDirectoryIndex(counts, math.MaxUint64); err == nil {
			t.Fatalf("accepted %v", counts)
		}
	}
	if _, err := buildDirectoryIndex(directoryCounts{"1": 2}, 1); err == nil {
		t.Fatal("accepted more messages than allocated IDs")
	}
	index, err := buildDirectoryIndex(directoryCounts{"1": 1, "2": 3, "2/001": 3}, 1026)
	if err != nil || !reflect.DeepEqual(index.leaves, []directoryCount{{"1", 1, 1}, {"2/001", 3, 4}}) {
		t.Fatalf("index = %+v, %v", index, err)
	}
}
