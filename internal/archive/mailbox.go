package archive

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

var ErrNoSuchMessage = errors.New("no such message")

// Snapshot shares an immutable deletion index and pins its messages to a Git
// commit. Creating it is O(1); resolving a message number is O(log ranges).
type Snapshot struct {
	LastID      uint64
	TotalOctets uint64
	deleted     deletedRanges
	commit      string
}

func (snapshot Snapshot) Count() uint64 { return snapshot.LastID - snapshot.deleted.count() }

// ID resolves a session's one-based message number to its permanent archive ID.
func (snapshot Snapshot) ID(number uint64) (uint64, bool) {
	if number == 0 || number > snapshot.Count() {
		return 0, false
	}
	return snapshot.deleted.selectID(number), true
}

func (snapshot Snapshot) contains(id uint64) bool {
	return id > 0 && id <= snapshot.LastID && !snapshot.deleted.contains(id)
}

// Walk visits live IDs in O(messages+ranges), skipping whole deleted runs.
func (snapshot Snapshot) Walk(visit func(number, id uint64) error) error {
	number, id, index := uint64(0), uint64(1), 0
	for id != 0 && id <= snapshot.LastID {
		if index < len(snapshot.deleted) && id == snapshot.deleted[index].Start {
			id = snapshot.deleted[index].End + 1
			index++
			continue
		}
		number++
		if err := visit(number, id); err != nil {
			return err
		}
		id++
	}
	return nil
}

func (s *Store) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Snapshot{LastID: s.state.LastID, TotalOctets: s.state.TotalOctets, deleted: s.state.DeletedRanges, commit: s.headID}
}

// Delete merges one batch into the current state, not the caller's older
// snapshot. Already deleted IDs are successful no-ops for concurrent sessions.
func (s *Store) Delete(ids []uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure != nil {
		return s.failure
	}
	ids = append([]uint64(nil), ids...)
	if !sort.SliceIsSorted(ids, func(i, j int) bool { return ids[i] < ids[j] }) {
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	}
	next := s.state
	var batch deletedRanges
	var paths []string
	var previous uint64
	deletedIndex := 0
	for _, id := range ids {
		if id == 0 || id > s.state.LastID {
			return ErrNoSuchMessage
		}
		if id == previous {
			continue
		}
		previous = id
		for deletedIndex < len(s.state.DeletedRanges) && s.state.DeletedRanges[deletedIndex].End < id {
			deletedIndex++
		}
		if deletedIndex < len(s.state.DeletedRanges) && s.state.DeletedRanges[deletedIndex].Start <= id {
			continue
		}
		size, err := s.SizeAt(Snapshot{LastID: s.state.LastID, TotalOctets: s.state.TotalOctets, deleted: s.state.DeletedRanges, commit: s.headID}, id)
		if err != nil {
			return err
		}
		if size > next.TotalOctets {
			return errors.New("message size exceeds mailbox octets")
		}
		next.TotalOctets -= size
		path, _ := messageRelativePath(id)
		paths = append(paths, filepath.ToSlash(path))
		if len(batch) > 0 && batch[len(batch)-1].End+1 == id {
			batch[len(batch)-1].End = id
		} else {
			batch = append(batch, deletedRange{Start: id, End: id})
		}
	}
	if len(paths) == 0 {
		return nil
	}
	next.DeletedRanges = s.state.DeletedRanges.merge(batch)
	if err := s.commitChanges(fmt.Sprintf("mail: delete %d messages", len(paths)), next, map[string][]byte{}, paths); err != nil {
		return err
	}
	s.state = next
	return nil
}

// SizeAt and OpenAt use the pinned commit, so another session's deletion cannot
// invalidate a message already visible in this session.
func (s *Store) SizeAt(snapshot Snapshot, id uint64) (uint64, error) {
	if id == 0 || id > snapshot.LastID || snapshot.commit == "" {
		return 0, ErrNoSuchMessage
	}
	path, _ := messageRelativePath(id)
	out, err := s.git(nil, "cat-file", "-s", snapshot.commit+":"+filepath.ToSlash(path))
	if err != nil {
		if snapshot.deleted.contains(id) {
			return 0, ErrNoSuchMessage
		}
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
}

func (s *Store) OpenAt(snapshot Snapshot, id uint64) (io.ReadCloser, error) {
	if !snapshot.contains(id) || snapshot.commit == "" {
		return nil, ErrNoSuchMessage
	}
	path, _ := messageRelativePath(id)
	return s.readBlob(snapshot.commit, filepath.ToSlash(path))
}

// Open and Size access the latest committed mailbox by permanent ID.
func (s *Store) Open(id uint64) (io.ReadCloser, error) { return s.OpenAt(s.Snapshot(), id) }
func (s *Store) Size(id uint64) (uint64, error)        { return s.SizeAt(s.Snapshot(), id) }
