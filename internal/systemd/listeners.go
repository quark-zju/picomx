// Package systemd provides the small subset of socket activation picomx uses.
package systemd

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
)

const listenFdsStart = 3

// Listeners converts all file descriptors passed by systemd into listeners.
func Listeners() ([]net.Listener, error) {
	pidRaw := os.Getenv("LISTEN_PID")
	fdsRaw := os.Getenv("LISTEN_FDS")
	if pidRaw == "" || fdsRaw == "" {
		return nil, errors.New("systemd socket activation environment is missing")
	}
	pid, err := strconv.Atoi(pidRaw)
	if err != nil {
		return nil, fmt.Errorf("parse LISTEN_PID: %w", err)
	}
	if pid != os.Getpid() {
		return nil, errors.New("LISTEN_PID does not match current process")
	}
	count, err := strconv.Atoi(fdsRaw)
	if err != nil {
		return nil, fmt.Errorf("parse LISTEN_FDS: %w", err)
	}
	if count <= 0 {
		return nil, errors.New("LISTEN_FDS must be greater than zero")
	}

	listeners := make([]net.Listener, 0, count)
	for index := 0; index < count; index++ {
		fd := listenFdsStart + index
		file := os.NewFile(uintptr(fd), fmt.Sprintf("LISTEN_FD_%d", index))
		listener, err := net.FileListener(file)
		_ = file.Close()
		if err != nil {
			for _, opened := range listeners {
				_ = opened.Close()
			}
			return nil, fmt.Errorf("convert listener fd %d: %w", fd, err)
		}
		listeners = append(listeners, listener)
	}
	return listeners, nil
}
