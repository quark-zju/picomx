package archive

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
)

func recoverMailboxTail(root string, state mailboxState) (mailboxState, error) {
	if state.LastID > 0 {
		info, err := statMessage(root, state.LastID)
		if err != nil {
			return mailboxState{}, fmt.Errorf("validate last indexed message %d: %w", state.LastID, err)
		}
		if !info.Mode().IsRegular() {
			return mailboxState{}, fmt.Errorf("last indexed message %d is not a regular file", state.LastID)
		}
	}
	if state.LastID == math.MaxUint64 {
		return state, nil
	}

	nextID := state.LastID + 1
	info, err := statMessage(root, nextID)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return mailboxState{}, fmt.Errorf("check recovery message %d: %w", nextID, err)
	}
	if !info.Mode().IsRegular() {
		return mailboxState{}, fmt.Errorf("recovery message %d is not a regular file", nextID)
	}
	if uint64(info.Size()) > math.MaxUint64-state.TotalOctets {
		return mailboxState{}, errors.New("recovered mailbox octet count overflow")
	}
	if nextID < math.MaxUint64 {
		if _, err := statMessage(root, nextID+1); err == nil {
			return mailboxState{}, errors.New("mailbox state trails more than one published message")
		} else if !errors.Is(err, os.ErrNotExist) {
			return mailboxState{}, fmt.Errorf("check message after recovery tail: %w", err)
		}
	}

	recovered := mailboxState{
		Version:     mailboxStateVersion,
		LastID:      nextID,
		TotalOctets: state.TotalOctets + uint64(info.Size()),
	}
	if err := writeMailboxState(root, recovered); err != nil {
		return mailboxState{}, fmt.Errorf("record recovered mailbox tail: %w", err)
	}
	return recovered, nil
}

func statMessage(root string, id uint64) (os.FileInfo, error) {
	relativePath, err := messageRelativePath(id)
	if err != nil {
		return nil, err
	}
	return os.Lstat(filepath.Join(root, relativePath))
}
