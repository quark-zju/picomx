package archive

import (
	"errors"
	"math"
	"sort"
)

// deletedRange is an inclusive, permanent tombstone. Ranges are ascending,
// disjoint and nonadjacent; AccCount includes every deletion through End.
type deletedRange struct {
	Start    uint64 `json:"start"`
	End      uint64 `json:"end"`
	AccCount uint64 `json:"acc_count"`
}

type deletedRanges []deletedRange

func (ranges deletedRanges) count() uint64 {
	if len(ranges) == 0 {
		return 0
	}
	return ranges[len(ranges)-1].AccCount
}

func (ranges deletedRanges) contains(id uint64) bool {
	index := sort.Search(len(ranges), func(i int) bool { return ranges[i].End >= id })
	return index < len(ranges) && ranges[index].Start <= id
}

// selectID maps the kth live message to its archive ID. End-AccCount is the
// number of live messages preceding a range, so these boundaries are ordered.
func (ranges deletedRanges) selectID(number uint64) uint64 {
	index := sort.Search(len(ranges), func(i int) bool { return ranges[i].End-ranges[i].AccCount >= number })
	if index == 0 {
		return number
	}
	return number + ranges[index-1].AccCount
}

func (ranges deletedRanges) validate(lastID uint64) error {
	var previousEnd, accumulated uint64
	for index, interval := range ranges {
		if interval.Start == 0 || interval.End < interval.Start || interval.End > lastID {
			return errors.New("deleted range is outside allocated message IDs")
		}
		if index > 0 && (previousEnd == math.MaxUint64 || interval.Start <= previousEnd+1) {
			return errors.New("deleted ranges must be sorted, disjoint and nonadjacent")
		}
		accumulated += interval.End - interval.Start + 1 // Disjoint ranges fit within lastID.
		if interval.AccCount != accumulated {
			return errors.New("invalid cumulative deletion count")
		}
		previousEnd = interval.End
	}
	return nil
}

// merge returns a new immutable array, sharing no writable storage with either
// input. Both inputs must already be sorted. It runs in O(len(a)+len(b)).
func (a deletedRanges) merge(b deletedRanges) deletedRanges {
	result := make(deletedRanges, 0, len(a)+len(b))
	for len(a) > 0 || len(b) > 0 {
		var next deletedRange
		if len(b) == 0 || (len(a) > 0 && a[0].Start <= b[0].Start) {
			next, a = a[0], a[1:]
		} else {
			next, b = b[0], b[1:]
		}
		if len(result) > 0 {
			last := &result[len(result)-1]
			if last.End == math.MaxUint64 || next.Start <= last.End+1 {
				if next.End > last.End {
					last.End = next.End
				}
				continue
			}
		}
		result = append(result, deletedRange{Start: next.Start, End: next.End})
	}
	var accumulated uint64
	for i := range result {
		accumulated += result[i].End - result[i].Start + 1
		result[i].AccCount = accumulated
	}
	return result
}
