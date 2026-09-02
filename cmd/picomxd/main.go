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

	"picomx/internal/archive"
	"picomx/internal/config"
	"picomx/internal/pop3"
	"picomx/internal/smtp"
	"picomx/internal/systemd"
	"picomx/internal/tlscert"
)

func main() {
	var (
		hostname          = flag.String("hostname", getenv("PICOMX_HOSTNAME", ""), "SMTP MX hostname")
		pop3Hostname      = flag.String("pop3-hostname", getenv("PICOMX_POP3_HOSTNAME", ""), "POP3S service hostname; defaults to SMTP hostname")
		domainsRaw        = flag.String("domains", getenv("PICOMX_DOMAINS", ""), "comma-separated recipient domains")
		archiveRoot       = flag.String("archive-root", getenv("PICOMX_ARCHIVE_ROOT", "/var/lib/picomx/messages"), "append-only message archive")
		listenAddress     = flag.String("listen", getenv("PICOMX_LISTEN", ""), "TCP address for development; default uses systemd socket activation")
		pop3ListenAddress = flag.String("pop3-listen", getenv("PICOMX_POP3_LISTEN", ""), "POP3S TCP address for development")
		certificateDir    = flag.String("cert-dir", getenv("PICOMX_CERT_DIR", ""), "directory containing hostname/fullchain.pem and privkey.pem")
		tlsHostnamesRaw   = flag.String("tls-hostnames", getenv("PICOMX_TLS_HOSTNAMES", ""), "comma-separated hostnames the shared certificate must cover")
		tlsReloadRaw      = flag.String("tls-reload-interval", getenv("PICOMX_TLS_RELOAD_INTERVAL", "5m"), "certificate content check interval")
		pop3Username      = flag.String("pop3-username", getenv("PICOMX_POP3_USERNAME", ""), "POP3 username")
		pop3PasswordHash  = flag.String("pop3-password-sha256", getenv("PICOMX_POP3_PASSWORD_SHA256", ""), "POP3 app-password SHA-256")
		pop3MaxConnsRaw   = flag.String("pop3-max-connections", getenv("PICOMX_POP3_MAX_CONNECTIONS", "16"), "maximum concurrent POP3S connections")
		pop3IdleRaw       = flag.String("pop3-idle-timeout", getenv("PICOMX_POP3_IDLE_TIMEOUT", "10m"), "POP3 session idle timeout")
		maxMessageRaw     = flag.String("max-message-bytes", getenv("PICOMX_MAX_MESSAGE_BYTES", "26214400"), "maximum RFC 5322 message bytes")
		maxRecipientsRaw  = flag.String("max-recipients", getenv("PICOMX_MAX_RECIPIENTS", "20"), "maximum recipients per message")
		maxConnectionsRaw = flag.String("max-connections", getenv("PICOMX_MAX_CONNECTIONS", "64"), "maximum concurrent SMTP connections")
		idleTimeoutRaw    = flag.String("idle-timeout", getenv("PICOMX_IDLE_TIMEOUT", "5m"), "SMTP session idle timeout")
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
	pop3MaxConnections, err := parsePositiveInt(*pop3MaxConnsRaw)
	exitOnError(logger, "parse pop3-max-connections", err)
	pop3IdleTimeout, err := time.ParseDuration(*pop3IdleRaw)
	exitOnError(logger, "parse pop3-idle-timeout", err)
	tlsReloadInterval, err := time.ParseDuration(*tlsReloadRaw)
	exitOnError(logger, "parse tls-reload-interval", err)
	if tlsReloadInterval <= 0 {
		exitOnError(logger, "parse tls-reload-interval", errors.New("must be greater than zero"))
	}

	listeners, err := openListeners(*listenAddress, *pop3ListenAddress)
	exitOnError(logger, "open listeners", err)
	defer closeListeners(listeners)
	pop3Enabled := hasProtocol(listeners, protocolPOP3S)
	pop3Host := strings.TrimSpace(*pop3Hostname)
	if pop3Host == "" {
		pop3Host = *hostname
	}

	var certificateManager *tlscert.Manager
	var tlsConfig *tls.Config
	if strings.TrimSpace(*certificateDir) != "" {
		tlsHostnames := splitDomains(*tlsHostnamesRaw)
		if len(tlsHostnames) == 0 {
			tlsHostnames = []string{*hostname, pop3Host}
		}
		certificateManager, err = tlscert.New(*certificateDir, tlsHostnames, logger)
		exitOnError(logger, "load TLS identity", err)
		tlsConfig = certificateManager.TLSConfig()
	} else if pop3Enabled {
		exitOnError(logger, "load TLS identity", errors.New("cert-dir is required when POP3S is enabled"))
	}

	store, err := archive.New(*archiveRoot)
	exitOnError(logger, "initialize archive", err)
	smtpServer, err := smtp.NewServer(smtp.Options{
		Hostname:       *hostname,
		Domains:        splitDomains(*domainsRaw),
		Delivery:       store,
		TLSConfig:      tlsConfig,
		MaxMessageSize: maxMessage,
		MaxRecipients:  maxRecipients,
		MaxConnections: maxConnections,
		IdleTimeout:    idleTimeout,
		Logger:         logger,
		Policy:         config.NewSMTPPolicy(),
	})
	exitOnError(logger, "configure SMTP server", err)
	credentials, err := pop3.NewCredentials(*pop3Username, *pop3PasswordHash)
	exitOnError(logger, "configure POP3 credentials", err)
	pop3Server, err := pop3.NewServer(pop3.Options{
		Hostname:       pop3Host,
		Mailbox:        store,
		Credentials:    credentials,
		MaxConnections: pop3MaxConnections,
		IdleTimeout:    pop3IdleTimeout,
		Logger:         logger,
	})
	exitOnError(logger, "configure POP3 server", err)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if certificateManager != nil {
		go certificateManager.RunReloadLoop(ctx, tlsReloadInterval)
	}
	errCh := make(chan error, len(listeners))
	for _, activated := range listeners {
		go func(activated serviceListener) {
			switch activated.protocol {
			case protocolSMTP:
				errCh <- smtpServer.Serve(activated.listener)
			case protocolPOP3S:
				errCh <- pop3Server.Serve(activated.listener, tlsConfig)
			}
		}(activated)
	}
	logger.Info("picomx started",
		"hostname", *hostname,
		"pop3_hostname", pop3Host,
		"domains", splitDomains(*domainsRaw),
		"archive_root", *archiveRoot,
		"starttls", tlsConfig != nil,
		"pop3s", pop3Enabled,
	)

	select {
	case <-ctx.Done():
	case serveErr := <-errCh:
		if serveErr != nil && !errors.Is(serveErr, net.ErrClosed) {
			exitOnError(logger, "serve protocol", serveErr)
		}
	}
}

type protocol string

const (
	protocolSMTP  protocol = "smtp"
	protocolPOP3S protocol = "pop3s"
)

type serviceListener struct {
	protocol protocol
	listener net.Listener
}

func openListeners(smtpAddress, pop3Address string) ([]serviceListener, error) {
	if smtpAddress == "" && pop3Address == "" {
		activated, err := systemd.Listeners()
		if err != nil {
			return nil, err
		}
		listeners := make([]serviceListener, 0, len(activated))
		for _, listener := range activated {
			kind, err := protocolForListener(listener)
			if err != nil {
				closeListeners(listeners)
				return nil, err
			}
			listeners = append(listeners, serviceListener{protocol: kind, listener: listener})
		}
		return listeners, nil
	}
	var listeners []serviceListener
	for _, configured := range []struct {
		kind    protocol
		address string
	}{{protocolSMTP, smtpAddress}, {protocolPOP3S, pop3Address}} {
		if configured.address == "" {
			continue
		}
		listener, err := net.Listen("tcp", configured.address)
		if err != nil {
			closeListeners(listeners)
			return nil, err
		}
		listeners = append(listeners, serviceListener{protocol: configured.kind, listener: listener})
	}
	return listeners, nil
}

func protocolForListener(listener net.Listener) (protocol, error) {
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return "", fmt.Errorf("cannot classify activated listener %q", listener.Addr())
	}
	switch address.Port {
	case 995:
		return protocolPOP3S, nil
	default:
		// The SMTP port is configured by systemd, so it need not be 25.
		// POP3S is the only activated protocol that requires a distinct
		// port to classify; every other activated TCP listener is SMTP.
		return protocolSMTP, nil
	}
}

func hasProtocol(listeners []serviceListener, kind protocol) bool {
	for _, listener := range listeners {
		if listener.protocol == kind {
			return true
		}
	}
	return false
}

func closeListeners(listeners []serviceListener) {
	for _, listener := range listeners {
		_ = listener.listener.Close()
	}
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
