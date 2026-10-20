package archive

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	mailboxStateVersion = 2
	mailboxStateName    = "state"
)

type mailboxState struct {
	Version       int           `json:"version"`
	LastID        uint64        `json:"last_id"`
	TotalOctets   uint64        `json:"total_octets"`
	DeletedRanges deletedRanges `json:"deleted_ranges,omitempty"`
}

func emptyMailboxState() mailboxState {
	return mailboxState{Version: mailboxStateVersion}
}

func readMailboxState(root string) (mailboxState, error) {
	file, err := os.Open(filepath.Join(root, mailboxStateName))
	if errors.Is(err, os.ErrNotExist) {
		return emptyMailboxState(), nil
	}
	if err != nil {
		return mailboxState{}, fmt.Errorf("open mailbox state: %w", err)
	}
	defer file.Close()

	return decodeMailboxState(file)
}

func decodeMailboxState(source io.Reader) (mailboxState, error) {
	decoder := json.NewDecoder(source)
	decoder.DisallowUnknownFields()
	var state mailboxState
	if err := decoder.Decode(&state); err != nil {
		return mailboxState{}, fmt.Errorf("decode mailbox state: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return mailboxState{}, fmt.Errorf("decode mailbox state: %w", err)
	}
	if state.Version != 1 && state.Version != mailboxStateVersion {
		return mailboxState{}, fmt.Errorf("unsupported mailbox state version %d", state.Version)
	}
	if state.Version == 1 && len(state.DeletedRanges) != 0 {
		return mailboxState{}, errors.New("legacy state cannot contain deleted ranges")
	}
	if err := state.DeletedRanges.validate(state.LastID); err != nil {
		return mailboxState{}, err
	}
	if state.LastID == state.DeletedRanges.count() && state.TotalOctets != 0 {
		return mailboxState{}, errors.New("empty mailbox state has nonzero total octets")
	}
	state.Version = mailboxStateVersion
	return state, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("mailbox state contains trailing JSON value")
		}
		return err
	}
	return nil
}

// writeMailboxState is only used while recovering a legacy file archive.
// Normal mailbox updates store state as a blob in the same commit as the mail.
func writeMailboxState(root string, state mailboxState) (err error) {
	if state.Version != mailboxStateVersion {
		return fmt.Errorf("refuse to write mailbox state version %d", state.Version)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create archive root: %w", err)
	}

	file, err := os.CreateTemp(root, ".state-*")
	if err != nil {
		return fmt.Errorf("create temporary mailbox state: %w", err)
	}
	tmpPath := file.Name()
	defer func() {
		_ = file.Close()
		_ = os.Remove(tmpPath)
	}()

	if err := json.NewEncoder(file).Encode(state); err != nil {
		return fmt.Errorf("encode mailbox state: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync mailbox state: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close mailbox state: %w", err)
	}
	if err := os.Rename(tmpPath, filepath.Join(root, mailboxStateName)); err != nil {
		return fmt.Errorf("publish mailbox state: %w", err)
	}

	directory, err := os.Open(root)
	if err != nil {
		return fmt.Errorf("open archive root for sync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync archive root: %w", err)
	}
	return nil
}
