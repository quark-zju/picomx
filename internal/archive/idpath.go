package archive

import (
	"errors"
	"fmt"
	"path/filepath"
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
