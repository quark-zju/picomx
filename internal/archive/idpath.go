package archive

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
)

const idRadix = uint64(1024)

func messageRelativePath(id uint64) (string, error) {
	if id == 0 {
		return "", errors.New("message ID must be greater than zero")
	}

	digits := make([]uint16, 0, 7)
	for id > 0 {
		digits = append(digits, uint16(id%idRadix))
		id /= idRadix
	}

	parts := make([]string, 0, len(digits)+1)
	parts = append(parts, fmt.Sprintf("%d", len(digits)))
	for index := len(digits) - 1; index >= 0; index-- {
		component := fmt.Sprintf("%03x", digits[index])
		if index == 0 {
			component += ".eml"
		}
		parts = append(parts, component)
	}
	return filepath.Join(parts...), nil
}

func messageIDFromPath(path string) (uint64, error) {
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return 0, errors.New("invalid message path")
	}
	var id uint64
	for _, part := range parts[1:] {
		digit, err := strconv.ParseUint(strings.TrimSuffix(part, ".eml"), 16, 10)
		if err != nil || id > (math.MaxUint64-digit)/idRadix {
			return 0, errors.New("invalid message path")
		}
		id = id*idRadix + digit
	}
	expected, err := messageRelativePath(id)
	if err != nil || filepath.ToSlash(expected) != path {
		return 0, errors.New("invalid message path: " + path)
	}
	return id, nil
}

// messagePath embeds immutable size metadata in the Git tree entry name.
func messagePath(id, size uint64) (string, error) {
	path, err := messageRelativePath(id)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(filepath.ToSlash(path), ".eml") + "-s" + strconv.FormatUint(size, 10) + ".eml", nil
}

func messageFromPath(path string) (Message, error) {
	stem := strings.TrimSuffix(path, ".eml")
	position := strings.LastIndex(stem, "-s")
	if position < 0 {
		return Message{}, errors.New("message path has no size")
	}
	id, err := messageIDFromPath(stem[:position] + ".eml")
	if err != nil {
		return Message{}, err
	}
	size, err := strconv.ParseUint(stem[position+2:], 10, 64)
	if err != nil {
		return Message{}, err
	}
	expected, err := messagePath(id, size)
	if err != nil || expected != path {
		return Message{}, errors.New("invalid message filename")
	}
	return Message{ID: id, Size: size, Path: path}, nil
}
