package archive

import (
	"errors"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type directoryCounts map[string]uint64

type directoryCount struct {
	path        string
	count       uint64
	accumulated uint64
}

// The index contains one entry per live leaf directory, not per message.
// It is immutable and shared by snapshots; a leaf has at most 1024 messages.
type directoryIndex struct {
	leaves []directoryCount
	total  uint64
}

func validDirectory(path string) (leaf bool, err error) {
	parts := strings.Split(path, "/")
	depth, err := strconv.Atoi(parts[0])
	if err != nil || depth < 1 || depth > 7 || parts[0] != strconv.Itoa(depth) || len(parts) > depth {
		return false, errors.New("invalid message directory")
	}
	var prefix uint64
	for i, part := range parts[1:] {
		digit, err := strconv.ParseUint(part, 16, 10)
		if err != nil || part != fmtDigit(digit) || (i == 0 && digit == 0) || prefix > (math.MaxUint64-digit)/idRadix {
			return false, errors.New("invalid message directory")
		}
		prefix = prefix*idRadix + digit
	}
	if len(parts) == depth && prefix > math.MaxUint64/idRadix {
		return false, errors.New("directory prefix exceeds ID space")
	}
	return len(parts) == depth, nil
}

func fmtDigit(digit uint64) string {
	text := strconv.FormatUint(digit, 16)
	return strings.Repeat("0", 3-len(text)) + text
}

func buildDirectoryIndex(counts directoryCounts, lastID uint64) (*directoryIndex, error) {
	index := &directoryIndex{}
	for path, count := range counts {
		leaf, err := validDirectory(path)
		if err != nil || count == 0 {
			return nil, errors.New("invalid directory count")
		}
		if leaf {
			if count > idRadix || (path == "1" && count == idRadix) {
				return nil, errors.New("leaf count exceeds directory fanout")
			}
			index.leaves = append(index.leaves, directoryCount{path: path, count: count})
		}
	}
	sort.Slice(index.leaves, func(i, j int) bool { return index.leaves[i].path < index.leaves[j].path })
	expected := directoryCounts{}
	for i := range index.leaves {
		leaf := &index.leaves[i]
		if leaf.count > lastID-index.total {
			return nil, errors.New("directory counts exceed allocated IDs")
		}
		index.total += leaf.count
		leaf.accumulated = index.total
		for path := leaf.path; path != "."; path = filepath.ToSlash(filepath.Dir(path)) {
			expected[path] += leaf.count
		}
	}
	if len(expected) != len(counts) {
		return nil, errors.New("directory counts are missing ancestors")
	}
	for path, count := range expected {
		if counts[path] != count {
			return nil, errors.New("directory count does not match descendants")
		}
	}
	return index, nil
}

func (index *directoryIndex) selectLeaf(number uint64) (directoryCount, uint64, bool) {
	if index == nil || number == 0 || number > index.total {
		return directoryCount{}, 0, false
	}
	position := sort.Search(len(index.leaves), func(i int) bool { return index.leaves[i].accumulated >= number })
	leaf := index.leaves[position]
	before := leaf.accumulated - leaf.count
	return leaf, number - before - 1, true
}

func cloneDirectoryCounts(counts directoryCounts) directoryCounts {
	clone := make(directoryCounts, len(counts))
	for path, count := range counts {
		clone[path] = count
	}
	return clone
}

func (counts directoryCounts) add(path string) {
	for path != "." {
		counts[path]++
		path = filepath.ToSlash(filepath.Dir(path))
	}
}

func (counts directoryCounts) remove(path string) error {
	for path != "." {
		count := counts[path]
		if count == 0 {
			return errors.New("directory count underflow")
		}
		if count == 1 {
			delete(counts, path)
		} else {
			counts[path] = count - 1
		}
		path = filepath.ToSlash(filepath.Dir(path))
	}
	return nil
}
