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
	certDir     string
	hostnames   []string
	logger      *slog.Logger
	now         func() time.Time
	reloadMu    sync.Mutex
	warningMu   sync.Mutex
	lastWarning string
	current     atomic.Pointer[loadedCertificate]
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
	manager := &Manager{certDir: certDir, hostnames: names, logger: logger, now: time.Now}
	manager.current.Store(&loaded)
	manager.reportExpiration()
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
	if now := m.now(); now.Before(next.leaf.NotBefore) || !now.Before(next.leaf.NotAfter) {
		return false, errors.New("replacement TLS certificate is not currently valid")
	}
	m.current.Store(&next)
	m.warningMu.Lock()
	m.lastWarning = ""
	m.warningMu.Unlock()
	m.logger.Info("TLS certificate reloaded", "directory", next.directory)
	m.reportExpiration()
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
			m.reportExpiration()
		}
	}
}

func (m *Manager) reportExpiration() {
	loaded := m.current.Load()
	if loaded == nil || loaded.leaf == nil {
		return
	}
	level := expirationLevel(loaded.leaf.NotAfter, m.now())
	if level == "" {
		return
	}
	key := loaded.leaf.NotAfter.UTC().Format(time.RFC3339Nano) + ":" + level
	m.warningMu.Lock()
	if m.lastWarning == key {
		m.warningMu.Unlock()
		return
	}
	m.lastWarning = key
	m.warningMu.Unlock()
	attributes := []any{"not_after", loaded.leaf.NotAfter.UTC(), "threshold", level}
	if level == "expired" {
		m.logger.Error("TLS certificate expired", attributes...)
		return
	}
	m.logger.Warn("TLS certificate expires soon", attributes...)
}

func expirationLevel(notAfter, now time.Time) string {
	remaining := notAfter.Sub(now)
	switch {
	case remaining <= 0:
		return "expired"
	case remaining <= 24*time.Hour:
		return "1d"
	case remaining <= 7*24*time.Hour:
		return "7d"
	case remaining <= 30*24*time.Hour:
		return "30d"
	default:
		return ""
	}
}
