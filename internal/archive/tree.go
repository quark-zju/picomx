package archive

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"sort"
	"strings"
)

type treeEntry struct{ mode, name, oid string }

// Git trees encode entries as mode SP name NUL followed by the raw object ID.
// The object ID length follows the repository hash algorithm (SHA-1/SHA-256).
func parseTree(data []byte, hashBytes int) ([]treeEntry, error) {
	var entries []treeEntry
	seen := map[string]bool{}
	for len(data) > 0 {
		end := bytes.IndexByte(data, 0)
		if end < 0 || len(data)-end-1 < hashBytes {
			return nil, errors.New("truncated Git tree entry")
		}
		mode, name, ok := strings.Cut(string(data[:end]), " ")
		if !ok || name == "" || strings.ContainsAny(name, "/\r\n") || name == "." || name == ".." || seen[name] || (mode != "40000" && mode != "100644") {
			return nil, errors.New("invalid archive tree entry")
		}
		seen[name] = true
		entries = append(entries, treeEntry{mode, name, hex.EncodeToString(data[end+1 : end+1+hashBytes])})
		data = data[end+1+hashBytes:]
	}
	return entries, nil
}

func (s *Store) tree(spec string) ([]treeEntry, string, error) {
	reader, err := s.readObject(spec, "tree")
	if err != nil {
		return nil, "", err
	}
	// All archive directories have bounded fanout. A corrupted repository must
	// not cause an unbounded allocation before entry validation.
	if reader.remaining > 256*1024 {
		_ = reader.Close()
		return nil, "", errors.New("archive tree exceeds bounded directory size")
	}
	data, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, "", err
	}
	entries, err := parseTree(data, len(reader.header.oid)/2)
	if uint64(len(entries)) > idRadix+3 {
		return nil, "", errors.New("archive directory exceeds fanout")
	}
	return entries, reader.header.oid, err
}

func (s *Store) directory(commit, path string) ([]Message, error) {
	header, err := s.objectInfo(commit+":"+path, "tree")
	if err != nil {
		return nil, err
	}
	key := directoryKey{path, header.oid}
	if messages, ok := s.cache.get(key); ok {
		return messages, nil
	}
	entries, _, err := s.tree(header.oid)
	if err != nil {
		return nil, err
	}
	messages := make([]Message, 0, len(entries))
	for _, entry := range entries {
		if entry.mode != "100644" {
			return nil, errors.New("message directory contains a subtree")
		}
		message, err := messageFromPath(path + "/" + entry.name)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	sort.Slice(messages, func(i, j int) bool { return messages[i].ID < messages[j].ID })
	for i, message := range messages {
		if i > 0 && messages[i-1].ID == message.ID {
			return nil, errors.New("duplicate message ID in directory")
		}
	}
	s.cache.put(key, messages)
	return messages, nil
}
