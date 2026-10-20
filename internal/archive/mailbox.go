package archive

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
)

var ErrNoSuchMessage = errors.New("no such message")

type Message struct {
	ID, Size uint64
	Path     string
}

// Snapshot shares the immutable directory-count index and a committed Git tree.
type Snapshot struct {
	LastID       uint64
	TotalOctets  uint64
	MessageCount uint64
	index        *directoryIndex
	commit       string
}

func (snapshot Snapshot) Count() uint64 { return snapshot.MessageCount }

func (s *Store) snapshotLocked() Snapshot {
	var count uint64
	if s.state.index != nil {
		count = s.state.index.total
	}
	return Snapshot{LastID: s.state.LastID, TotalOctets: s.state.TotalOctets, MessageCount: count, index: s.state.index, commit: s.headID}
}

func (s *Store) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

// Lookup locates a directory by cumulative live counts, then selects within its
// bounded, cached listing. No per-message index is built when a session logs in.
func (s *Store) Lookup(snapshot Snapshot, number uint64) (Message, error) {
	leaf, offset, ok := snapshot.index.selectLeaf(number)
	if !ok {
		return Message{}, ErrNoSuchMessage
	}
	messages, err := s.directory(snapshot.commit, leaf.path)
	if err != nil {
		return Message{}, err
	}
	if uint64(len(messages)) != leaf.count {
		return Message{}, errors.New("directory listing does not match count")
	}
	return messages[offset], nil
}

func (s *Store) messageByID(snapshot Snapshot, id uint64) (Message, error) {
	if snapshot.index == nil || id == 0 || id > snapshot.LastID {
		return Message{}, ErrNoSuchMessage
	}
	legacy, _ := messageRelativePath(id)
	path := filepath.ToSlash(filepath.Dir(legacy))
	position := sort.Search(len(snapshot.index.leaves), func(i int) bool { return snapshot.index.leaves[i].path >= path })
	if position == len(snapshot.index.leaves) || snapshot.index.leaves[position].path != path {
		return Message{}, ErrNoSuchMessage
	}
	messages, err := s.directory(snapshot.commit, path)
	if err != nil {
		return Message{}, err
	}
	if uint64(len(messages)) != snapshot.index.leaves[position].count {
		return Message{}, errors.New("directory listing does not match count")
	}
	position = sort.Search(len(messages), func(i int) bool { return messages[i].ID >= id })
	if position == len(messages) || messages[position].ID != id {
		return Message{}, ErrNoSuchMessage
	}
	return messages[position], nil
}

// ListAt streams directories in numeric archive-ID order. Each leaf is bounded
// to 1024 entries; the shared LRU avoids repeated object reads and parsing.
func (s *Store) ListAt(snapshot Snapshot, visit func(number, id, size uint64) error) error {
	if snapshot.index == nil {
		return ErrNoSuchMessage
	}
	var number uint64
	for _, leaf := range snapshot.index.leaves {
		messages, err := s.directory(snapshot.commit, leaf.path)
		if err != nil {
			return err
		}
		if uint64(len(messages)) != leaf.count {
			return errors.New("directory listing does not match count")
		}
		for _, message := range messages {
			if message.ID > snapshot.LastID {
				return errors.New("directory contains unallocated message ID")
			}
			number++
			if err := visit(number, message.ID, message.Size); err != nil {
				return err
			}
		}
	}
	return nil
}

// Delete merges removals into the latest tree. An ID absent from its leaf is
// already deleted; no persistent per-message tombstone is necessary.
func (s *Store) Delete(ids []uint64) error { return s.DeleteAs(ids, "") }

// DeleteAs records the authenticated POP3 user in the generated commit message.
// The username is descriptive metadata, never a Git author identity or argument.
func (s *Store) DeleteAs(ids []uint64, username string) error {
	ids = append([]uint64(nil), ids...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for {
		snapshot := s.Snapshot()
		var messages []Message
		var previous uint64
		for _, id := range ids {
			if id == 0 || id > snapshot.LastID {
				return ErrNoSuchMessage
			}
			if id == previous {
				continue
			}
			previous = id
			message, err := s.messageByID(snapshot, id)
			if errors.Is(err, ErrNoSuchMessage) {
				continue
			}
			if err != nil {
				return err
			}
			messages = append(messages, message)
		}
		// Resolving paths may wait for a streaming reader. Never hold the state
		// lock across that wait; retry against a newer snapshot if a writer ran.
		s.mu.Lock()
		if s.headID != snapshot.commit {
			s.mu.Unlock()
			continue
		}
		err := s.deleteLocked(messages, username)
		s.mu.Unlock()
		return err
	}
}

func (s *Store) deleteLocked(messages []Message, username string) error {
	if s.failure != nil {
		return s.failure
	}
	if len(messages) == 0 {
		return nil
	}
	next := s.state
	next.Directories = cloneDirectoryCounts(s.state.Directories)
	paths := make([]string, 0, len(messages))
	for _, message := range messages {
		if message.Size > next.TotalOctets {
			return errors.New("message size exceeds mailbox octets")
		}
		next.TotalOctets -= message.Size
		if err := next.Directories.remove(filepath.ToSlash(filepath.Dir(message.Path))); err != nil {
			return err
		}
		paths = append(paths, message.Path)
	}
	var err error
	next.index, err = buildDirectoryIndex(next.Directories, next.LastID)
	if err != nil {
		return err
	}
	title := fmt.Sprintf("mail: delete %d messages", len(paths))
	if username != "" {
		// Preserve ordinary names verbatim. Quote names containing controls so a
		// configured username cannot forge extra metadata lines in the commit body.
		printable := strconv.QuoteToGraphic(username)
		if printable == "\""+username+"\"" {
			printable = username
		}
		title += "\n\nPOP3 user: " + printable
	}
	if err := s.commitChanges(title, next, map[string][]byte{}, paths); err != nil {
		return err
	}
	s.state = next
	return nil
}

func (s *Store) SizeAt(snapshot Snapshot, id uint64) (uint64, error) {
	message, err := s.messageByID(snapshot, id)
	return message.Size, err
}

func (s *Store) OpenMessage(snapshot Snapshot, message Message) (io.ReadCloser, error) {
	parsed, err := messageFromPath(message.Path)
	if err != nil || parsed != message || message.ID > snapshot.LastID {
		return nil, ErrNoSuchMessage
	}
	reader, err := s.readBlob(snapshot.commit, message.Path)
	if err != nil {
		return nil, err
	}
	if reader.remaining != int64(message.Size) {
		_ = reader.Close()
		return nil, errors.New("message size does not match filename")
	}
	return reader, nil
}

func (s *Store) OpenAt(snapshot Snapshot, id uint64) (io.ReadCloser, error) {
	message, err := s.messageByID(snapshot, id)
	if err != nil {
		return nil, err
	}
	return s.OpenMessage(snapshot, message)
}

func (s *Store) Open(id uint64) (io.ReadCloser, error) { return s.OpenAt(s.Snapshot(), id) }
func (s *Store) Size(id uint64) (uint64, error)        { return s.SizeAt(s.Snapshot(), id) }
