package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"picomail/internal/archive"
	"picomail/internal/smtp"
	"picomail/internal/systemd"
)

func main() {
	var (
		hostname          = flag.String("hostname", getenv("PICOMAIL_HOSTNAME", ""), "SMTP MX hostname")
		domainsRaw        = flag.String("domains", getenv("PICOMAIL_DOMAINS", ""), "comma-separated recipient domains")
		archiveRoot       = flag.String("archive-root", getenv("PICOMAIL_ARCHIVE_ROOT", "/var/lib/picomail/messages"), "append-only message archive")
		listenAddress     = flag.String("listen", getenv("PICOMAIL_LISTEN", ""), "TCP address for development; default uses systemd socket activation")
		certificatePath   = flag.String("tls-cert", getenv("PICOMAIL_TLS_CERT", ""), "STARTTLS certificate PEM")
		privateKeyPath    = flag.String("tls-key", getenv("PICOMAIL_TLS_KEY", ""), "STARTTLS private key PEM")
		maxMessageRaw     = flag.String("max-message-bytes", getenv("PICOMAIL_MAX_MESSAGE_BYTES", "26214400"), "maximum RFC 5322 message bytes")
		maxRecipientsRaw  = flag.String("max-recipients", getenv("PICOMAIL_MAX_RECIPIENTS", "20"), "maximum recipients per message")
		maxConnectionsRaw = flag.String("max-connections", getenv("PICOMAIL_MAX_CONNECTIONS", "64"), "maximum concurrent SMTP connections")
		idleTimeoutRaw    = flag.String("idle-timeout", getenv("PICOMAIL_IDLE_TIMEOUT", "5m"), "SMTP session idle timeout")
	)
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	maxMessage, err := parsePositiveInt64(*maxMessageRaw)
	exitOnError(logger, "parse max-message-bytes", err)
	maxRecipients, err := parsePositiveInt(*maxRecipientsRaw)
	exitOnError(logger, "parse max-recipients", err)
	maxConnections, err := parsePositiveInt(*maxConnectionsRaw)
	exitOnError(logger, "parse max-connections", err)
	idleTimeout, err := time.ParseDuration(*idleTimeoutRaw)
	exitOnError(logger, "parse idle-timeout", err)
	if idleTimeout <= 0 {
		exitOnError(logger, "parse idle-timeout", errors.New("must be greater than zero"))
	}

	tlsConfig, err := loadTLSConfig(*certificatePath, *privateKeyPath)
	exitOnError(logger, "load STARTTLS identity", err)
	store, err := archive.New(*archiveRoot)
	exitOnError(logger, "initialize archive", err)
	server, err := smtp.NewServer(smtp.Options{
		Hostname:       *hostname,
		Domains:        splitDomains(*domainsRaw),
		Delivery:       store,
		TLSConfig:      tlsConfig,
		MaxMessageSize: maxMessage,
		MaxRecipients:  maxRecipients,
		MaxConnections: maxConnections,
		IdleTimeout:    idleTimeout,
		Logger:         logger,
	})
	exitOnError(logger, "configure SMTP server", err)

	listeners, err := openListeners(*listenAddress)
	exitOnError(logger, "open SMTP listeners", err)
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, len(listeners))
	for _, listener := range listeners {
		go func(listener net.Listener) {
			errCh <- server.Serve(listener)
		}(listener)
	}
	logger.Info("SMTP server started",
		"hostname", *hostname,
		"domains", splitDomains(*domainsRaw),
		"archive_root", *archiveRoot,
		"starttls", tlsConfig != nil,
	)

	select {
	case <-ctx.Done():
	case serveErr := <-errCh:
		if serveErr != nil && !errors.Is(serveErr, net.ErrClosed) {
			exitOnError(logger, "serve SMTP", serveErr)
		}
	}
}

func openListeners(address string) ([]net.Listener, error) {
	if address == "" {
		return systemd.Listeners()
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	return []net.Listener{listener}, nil
}

func loadTLSConfig(certificatePath, privateKeyPath string) (*tls.Config, error) {
	if certificatePath == "" && privateKeyPath == "" {
		return nil, nil
	}
	if certificatePath == "" || privateKeyPath == "" {
		return nil, errors.New("tls-cert and tls-key must be provided together")
	}
	certificate, err := tls.LoadX509KeyPair(certificatePath, privateKeyPath)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

func splitDomains(raw string) []string {
	var domains []string
	for _, domain := range strings.Split(raw, ",") {
		if domain = strings.TrimSpace(domain); domain != "" {
			domains = append(domains, domain)
		}
	}
	return domains
}

func parsePositiveInt(raw string) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, errors.New("must be a positive integer")
	}
	return value, nil
}

func parsePositiveInt64(raw string) (int64, error) {
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return 0, errors.New("must be a positive integer")
	}
	return value, nil
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func exitOnError(logger *slog.Logger, action string, err error) {
	if err == nil {
		return
	}
	logger.Error(action, "error", fmt.Sprint(err))
	os.Exit(1)
}
