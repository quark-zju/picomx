package archive

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func (s *Store) gitCommand(args ...string) *exec.Cmd {
	options := []string{"--git-dir=" + s.root,
		"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
		"-c", "core.attributesFile=/dev/null", "-c", "core.autocrlf=false",
		"-c", "commit.gpgSign=false", "-c", "gc.auto=0", "-c", "maintenance.auto=false",
		"-c", "core.fsync=committed", "-c", "core.fsyncMethod=fsync"}
	cmd := exec.Command("git", append(options, args...)...)
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, "GIT_") {
			cmd.Env = append(cmd.Env, variable)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_NO_REPLACE_OBJECTS=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=picomx", "GIT_AUTHOR_EMAIL=picomx@localhost",
		"GIT_COMMITTER_NAME=picomx", "GIT_COMMITTER_EMAIL=picomx@localhost")

	return cmd
}

func (s *Store) git(input io.Reader, args ...string) ([]byte, error) {
	cmd := s.gitCommand(args...)
	cmd.Stdin = input
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func (s *Store) head() (string, error) {
	out, err := s.git(nil, "rev-parse", "--verify", "HEAD")
	return strings.TrimSpace(string(out)), err
}

func (s *Store) initializeGit() error {
	version, err := s.git(nil, "version")
	if err != nil {
		return fmt.Errorf("Git 2.36 or newer is required: %w", err)
	}
	fields := strings.Fields(string(version))
	if len(fields) < 3 {
		return errors.New("cannot determine Git version")
	}
	numbers := strings.Split(fields[2], ".")
	if len(numbers) < 2 {
		return errors.New("cannot determine Git version")
	}
	major, majorErr := strconv.Atoi(numbers[0])
	minor, minorErr := strconv.Atoi(numbers[1])
	if majorErr != nil || minorErr != nil || major < 2 || (major == 2 && minor < 36) {
		return errors.New("Git 2.36 or newer is required")
	}
	if _, err := os.Lstat(filepath.Join(s.root, ".git")); err == nil {
		return errors.New("archive has a working tree; clone it with git clone --bare to a new directory and set archive-root to that directory")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := os.Lstat(filepath.Join(s.root, "HEAD")); errors.Is(err, os.ErrNotExist) {
		if _, err := s.git(nil, "init", "--bare", "--template=", "--initial-branch=main"); err != nil {
			return err
		}
		if err := syncDirectory(s.root); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	bare, err := s.git(nil, "rev-parse", "--is-bare-repository")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(bare)) != "true" {
		return errors.New("archive must be a bare Git repository")
	}
	if err := os.MkdirAll(filepath.Join(s.root, "tmp"), 0o700); err != nil {
		return err
	}

	head, err := s.head()
	if err != nil {
		// A missing HEAD is legitimate only for a new, unborn branch.
		ref, refErr := s.git(nil, "symbolic-ref", "HEAD")
		if refErr != nil {
			return err
		}
		refs, refErr := s.git(nil, "for-each-ref", "--format=%(refname)", strings.TrimSpace(string(ref)))
		if refErr != nil || len(refs) != 0 {
			return err
		}
		head = ""
	}
	ref, err := s.git(nil, "symbolic-ref", "HEAD")
	if err != nil {
		return errors.New("archive HEAD must point to a branch")
	}
	s.refName = strings.TrimSpace(string(ref))
	if !strings.HasPrefix(s.refName, "refs/heads/") || strings.ContainsAny(s.refName, "\r\n") {
		return errors.New("invalid archive branch")
	}
	s.headID = head
	if head == "" {
		state, err := readMailboxState(s.root)
		if err != nil {
			return err
		}
		state, err = recoverMailboxTail(s.root, state)
		if err != nil {
			return err
		}
		s.state = state
		changes := map[string][]byte{}
		// Import every live legacy message before enabling deletion.
		snapshot := Snapshot{LastID: state.LastID, deleted: state.DeletedRanges}
		var total uint64
		if err := snapshot.Walk(func(number, id uint64) error {
			path, _ := messageRelativePath(id)
			info, err := statMessage(s.root, id)
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("legacy message %d is not regular", id)
			}
			size := uint64(info.Size())
			if size > ^uint64(0)-total {
				return errors.New("legacy archive octet overflow")
			}
			total += size
			changes[filepath.ToSlash(path)] = nil
			return nil
		}); err != nil {
			return err
		}
		if total != state.TotalOctets {
			return errors.New("legacy archive octets do not match state")
		}
		if err := s.commitChanges("mail: initialize archive", state, changes, nil); err != nil {
			return err
		}
	} else {
		data, err := s.git(nil, "cat-file", "blob", head+":state")
		if err != nil {
			return fmt.Errorf("read committed mailbox state: %w", err)
		}
		state, err := decodeMailboxState(bytes.NewReader(data))
		if err != nil {
			return err
		}
		s.state = state
	}

	if err := s.validateTree(); err != nil {
		return err
	}
	// Version 1 is normalized by the decoder and persisted on the next write.

	return nil
}

func (s *Store) validateTree() error {
	out, err := s.git(nil, "ls-tree", "-r", "-l", "-z", "HEAD")
	if err != nil {
		return err
	}
	var total, count, previousID uint64
	deletedIndex := 0
	for _, entry := range bytes.Split(out, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		metadata, path, ok := strings.Cut(string(entry), "\t")
		if !ok {
			return errors.New("invalid archive tree entry")
		}
		if path == "state" || path == ".gitignore" || path == ".gitattributes" {
			continue
		}
		id, err := messageIDFromPath(path)
		if err != nil {
			return err
		}
		fields := strings.Fields(metadata)
		if len(fields) != 4 || fields[0] != "100644" || fields[1] != "blob" {
			return fmt.Errorf("invalid message entry %s", path)
		}
		size, err := strconv.ParseUint(fields[3], 10, 64)
		if err != nil {
			return err
		}
		// Canonical base-1024 paths sort in ID order in Git's tree traversal.
		for deletedIndex < len(s.state.DeletedRanges) && s.state.DeletedRanges[deletedIndex].End < id {
			deletedIndex++
		}
		deleted := deletedIndex < len(s.state.DeletedRanges) && s.state.DeletedRanges[deletedIndex].Start <= id
		if id <= previousID || id > s.state.LastID || deleted || size > ^uint64(0)-total {
			return errors.New("archive IDs or octets exceed state")
		}
		previousID = id
		total += size
		count++
	}
	if total != s.state.TotalOctets {
		return errors.New("archive octets do not match state")
	}
	if count != s.state.LastID-s.state.DeletedRanges.count() {
		return errors.New("archive message count does not match state")
	}
	return nil
}
