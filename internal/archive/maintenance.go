package archive

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

const DefaultMaintenanceInterval = 6 * time.Hour
const maintenanceCommitThreshold = 256
const maintenanceTimeout = 5 * time.Minute

var ErrMaintenanceBusy = errors.New("Git archive is busy")

// Maintain coordinates a foreground GC with all in-process Git users. A leased
// POP3 reader postpones maintenance instead of blocking SMTP behind that reader.
func (s *Store) Maintain(ctx context.Context) error {
	if !s.readMu.TryLock() {
		return ErrMaintenanceBusy
	}
	defer s.readMu.Unlock()
	if !s.mu.TryLock() {
		return ErrMaintenanceBusy
	}
	defer s.mu.Unlock()
	if !s.gitMu.TryLock() {
		return ErrMaintenanceBusy
	}
	defer s.gitMu.Unlock()
	if s.closed || s.readClosed {
		return errors.New("archive is closed")
	}
	if s.failure != nil {
		return s.failure
	}
	if s.lastMaintenanceHead == s.headID {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.idleGeneration++
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
	s.readIdleGeneration++
	if s.readIdleTimer != nil {
		s.readIdleTimer.Stop()
		s.readIdleTimer = nil
	}
	if err := s.closeImporterLocked(); err != nil {
		s.importError = err
		s.failure = err
		return err
	}
	if err := s.closeReaderLocked(); err != nil {
		return err
	}
	// Non-auto GC runs in the foreground. gc.autoDetach=false also supports
	// Git 2.36, which predates the explicit --no-detach option.
	if _, err := s.runGit(ctx, maintenanceTimeout, nil, "-c", "gc.autoDetach=false", "gc", "--prune=never"); err != nil {
		return err
	}
	s.lastMaintenanceHead = s.headID
	s.commitsSinceMaintenance = 0
	return nil
}

// RunMaintenance runs until cancellation; ordinary Git commands keep implicit
// auto maintenance disabled so no detached worker escapes this coordination.
func (s *Store) RunMaintenance(ctx context.Context, interval time.Duration, logger *slog.Logger) {
	if interval <= 0 {
		interval = DefaultMaintenanceInterval
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-s.maintenanceRequests:
		}
		err := s.Maintain(ctx)
		if ctx.Err() != nil {
			return
		}
		retry := interval
		switch {
		case errors.Is(err, ErrMaintenanceBusy):
			retry = time.Minute
		case err != nil:
			logger.Warn("Git maintenance failed", "error", err)
			retry = time.Hour
		default:
			logger.Info("Git maintenance completed")
		}
		timer.Reset(retry)
	}
}
