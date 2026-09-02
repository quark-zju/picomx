package tlscert

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Manager owns the one certificate shared by SMTP STARTTLS and POP3S.
type Manager struct {
	certDir   string
	hostnames []string
	logger    *slog.Logger
	reloadMu  sync.Mutex
	current   atomic.Pointer[loadedCertificate]
}

// New loads and validates the initial fixed certificate.
func New(certDir string, hostnames []string, logger *slog.Logger) (*Manager, error) {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	names, err := normalizeHostnames(hostnames)
	if err != nil {
		return nil, err
	}
	loaded, err := loadCertificate(certDir, names)
	if err != nil {
		return nil, err
	}
	manager := &Manager{certDir: certDir, hostnames: names, logger: logger}
	manager.current.Store(&loaded)
	return manager, nil
}

// TLSConfig returns a configuration that accepts clients with or without SNI.
func (m *Manager) TLSConfig() *tls.Config {
	return &tls.Config{
		GetCertificate: m.GetCertificate,
		MinVersion:     tls.VersionTLS12,
	}
}

// GetCertificate implements tls.Config.GetCertificate.
func (m *Manager) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	loaded := m.current.Load()
	if loaded == nil || loaded.certificate == nil {
		return nil, errors.New("TLS certificate is unavailable")
	}
	return loaded.certificate, nil
}

// Reload discovers the configured certificate again and swaps it after validation.
func (m *Manager) Reload() (bool, error) {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()

	next, err := loadCertificate(m.certDir, m.hostnames)
	if err != nil {
		return false, err
	}
	current := m.current.Load()
	if current != nil && current.hash == next.hash && current.directory == next.directory {
		return false, nil
	}
	m.current.Store(&next)
	m.logger.Info("TLS certificate reloaded", "directory", next.directory)
	return true, nil
}

// RunReloadLoop checks certificate content until ctx is canceled.
func (m *Manager) RunReloadLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := m.Reload(); err != nil {
				m.logger.Error("reload TLS certificate", "error", err)
			}
		}
	}
}
