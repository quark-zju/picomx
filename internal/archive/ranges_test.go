package archive

import (
	"math"
	"reflect"
	"testing"
)

func TestRangesSelectAndWalkAgainstEverySmallMailbox(t *testing.T) {
	t.Parallel()
	const lastID = 12
	for mask := 0; mask < 1<<lastID; mask++ {
		var a, b deletedRanges
		var live []uint64
		for id := uint64(1); id <= lastID; id++ {
			if mask&(1<<(id-1)) == 0 {
				live = append(live, id)
				continue
			}
			interval := deletedRange{Start: id, End: id}
			if id%2 == 0 {
				a = append(a, interval)
			} else {
				b = append(b, interval)
			}
		}
		merged := a.merge(b)
		if err := merged.validate(lastID); err != nil {
			t.Fatalf("mask %x: %v", mask, err)
		}
		snapshot := Snapshot{LastID: lastID, deleted: merged}
		if snapshot.Count() != uint64(len(live)) {
			t.Fatalf("mask %x: count %d", mask, snapshot.Count())
		}
		for index, id := range live {
			if got, ok := snapshot.ID(uint64(index + 1)); !ok || got != id {
				t.Fatalf("mask %x: select %d = %d, %v; want %d", mask, index+1, got, ok, id)
			}
		}
		for _, number := range []uint64{0, snapshot.Count() + 1} {
			if _, ok := snapshot.ID(number); ok {
				t.Fatalf("mask %x: accepted number %d", mask, number)
			}
		}
		for id := uint64(1); id <= lastID; id++ {
			want := mask&(1<<(id-1)) != 0
			if merged.contains(id) != want {
				t.Fatalf("mask %x: contains %d", mask, id)
			}
		}
		var walked []uint64
		if err := snapshot.Walk(func(number, id uint64) error {
			if number != uint64(len(walked)+1) {
				t.Fatalf("nonconsecutive walk number %d", number)
			}
			walked = append(walked, id)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(walked, live) {
			t.Fatalf("mask %x: walk %v, want %v", mask, walked, live)
		}
	}
}

func TestRangesMergeOverlapsAndPreservesInputs(t *testing.T) {
	t.Parallel()
	a := deletedRanges{{Start: 3, End: 5, AccCount: 3}, {Start: 8, End: 9, AccCount: 5}}
	b := deletedRanges{{Start: 1, End: 3}, {Start: 5, End: 8}, {Start: 20, End: 30}}
	want := deletedRanges{{Start: 1, End: 9, AccCount: 9}, {Start: 20, End: 30, AccCount: 20}}
	got := a.merge(b)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge = %v, want %v", got, want)
	}
	got[0].End = 15
	if a[0].End != 5 || b[0].End != 3 {
		t.Fatal("merge mutated input storage")
	}
}

func TestRangesAtUint64Boundary(t *testing.T) {
	t.Parallel()
	for _, ranges := range []deletedRanges{
		nil,
		{{Start: 1, End: math.MaxUint64, AccCount: math.MaxUint64}},
		{{Start: 1, End: math.MaxUint64 - 1, AccCount: math.MaxUint64 - 1}},
		{{Start: math.MaxUint64, End: math.MaxUint64, AccCount: 1}},
	} {
		if err := ranges.validate(math.MaxUint64); err != nil {
			t.Fatal(err)
		}
		snapshot := Snapshot{LastID: math.MaxUint64, deleted: ranges}
		if snapshot.Count() > 0 {
			got, ok := snapshot.ID(snapshot.Count())
			want := uint64(math.MaxUint64)
			if ranges.contains(want) {
				want--
			}
			if !ok || got != want {
				t.Fatalf("ranges %v: last = %d, %v", ranges, got, ok)
			}
		}
		if snapshot.Count() <= 1 {
			var count uint64
			if err := snapshot.Walk(func(number, id uint64) error { count++; return nil }); err != nil {
				t.Fatal(err)
			}
			if count != snapshot.Count() {
				t.Fatalf("walk count %d", count)
			}
		}
	}
}

func TestRangesRejectInvalidMetadata(t *testing.T) {
	t.Parallel()
	for _, ranges := range []deletedRanges{
		{{Start: 0, End: 1, AccCount: 2}},
		{{Start: 3, End: 2}},
		{{Start: 1, End: 11, AccCount: 11}},
		{{Start: 1, End: 1, AccCount: 0}},
		{{Start: 3, End: 3, AccCount: 1}, {Start: 1, End: 1, AccCount: 2}},
		{{Start: 1, End: 3, AccCount: 3}, {Start: 3, End: 5, AccCount: 6}},
		{{Start: 1, End: 3, AccCount: 3}, {Start: 4, End: 5, AccCount: 5}},
	} {
		if err := ranges.validate(10); err == nil {
			t.Fatalf("accepted invalid ranges %v", ranges)
		}
	}
}
