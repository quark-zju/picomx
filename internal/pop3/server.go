// Package pop3 implements picomx's deliberately read-only POP3 surface.
package pop3

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"picomx/internal/archive"
)

const (
	minimumIdleTimeout = 10 * time.Minute
	maxCommandBytes    = 255
)

type snapshotter interface {
	Snapshot() archive.Snapshot
}

// Options configures the POP3 protocol state machine.
type Options struct {
	Hostname        string
	Mailbox         snapshotter
	Credentials     Credentials
	IdleTimeout     time.Duration
	MaxAuthFailures int
	Logger          *slog.Logger
}

// Server serves one read-only maildrop.
type Server struct {
	hostname        string
	mailbox         snapshotter
	credentials     Credentials
	idleTimeout     time.Duration
	maxAuthFailures int
	logger          *slog.Logger
}

// NewServer validates POP3 options.
func NewServer(options Options) (*Server, error) {
	if strings.TrimSpace(options.Hostname) == "" {
		return nil, errors.New("POP3 hostname is required")
	}
	if options.Mailbox == nil {
		return nil, errors.New("POP3 mailbox is required")
	}
	if options.IdleTimeout == 0 {
		options.IdleTimeout = minimumIdleTimeout
	}
	if options.IdleTimeout < minimumIdleTimeout {
		return nil, fmt.Errorf("POP3 idle timeout must be at least %s", minimumIdleTimeout)
	}
	if options.MaxAuthFailures <= 0 {
		options.MaxAuthFailures = 3
	}
	if options.Logger == nil {
		options.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &Server{
		hostname:        strings.TrimSpace(options.Hostname),
		mailbox:         options.Mailbox,
		credentials:     options.Credentials,
		idleTimeout:     options.IdleTimeout,
		maxAuthFailures: options.MaxAuthFailures,
		logger:          options.Logger,
	}, nil
}

func (s *Server) serveConn(conn net.Conn, tlsVersion uint16) {
	defer conn.Close()
	reader := bufio.NewReaderSize(conn, maxCommandBytes+1)
	writer := bufio.NewWriter(conn)
	remoteIP := clientIP(conn.RemoteAddr())
	var username string
	var authenticated bool
	var snapshot archive.Snapshot
	authFailures := 0

	if !s.reply(conn, writer, "+OK "+s.hostname+" picomx ready") {
		return
	}
	for {
		if err := conn.SetDeadline(time.Now().Add(s.idleTimeout)); err != nil {
			return
		}
		line, err := readCommand(reader)
		if err != nil {
			if errors.Is(err, errCommandTooLong) {
				s.reply(conn, writer, "-ERR command too long")
			}
			return
		}
		command, argument := splitCommand(line)
		switch command {
		case "CAPA":
			if argument != "" {
				s.reply(conn, writer, "-ERR invalid arguments")
				continue
			}
			s.replyMulti(conn, writer, []string{"+OK Capability list follows", "USER", "IMPLEMENTATION picomx"})
		case "USER":
			if authenticated || strings.TrimSpace(argument) == "" {
				s.reply(conn, writer, "-ERR command not valid now")
				continue
			}
			username = strings.TrimSpace(argument)
			s.reply(conn, writer, "+OK send PASS")
		case "PASS":
			if authenticated || username == "" || argument == "" {
				s.reply(conn, writer, "-ERR command not valid now")
				continue
			}
			if !s.credentials.authenticate(username, argument) {
				authFailures++
				s.logger.Warn("POP3 authentication failed",
					"protocol", "pop3",
					"client_ip", remoteIP,
					"tls_version", tlsVersion,
					"ban_candidate", true,
					"ban_reason", "authentication_failed",
				)
				username = ""
				s.reply(conn, writer, "-ERR authentication failed")
				if authFailures >= s.maxAuthFailures {
					return
				}
				continue
			}
			authenticated = true
			snapshot = s.mailbox.Snapshot()
			s.reply(conn, writer, fmt.Sprintf("+OK maildrop ready with %d messages", snapshot.LastID))
		case "QUIT":
			if argument != "" {
				s.reply(conn, writer, "-ERR invalid arguments")
				continue
			}
			s.reply(conn, writer, "+OK bye")
			return
		default:
			s.reply(conn, writer, "-ERR command not implemented")
		}
	}
}

var errCommandTooLong = errors.New("POP3 command too long")

func readCommand(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) || len(line) > maxCommandBytes {
		return "", errCommandTooLong
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r"), nil
}

func splitCommand(line string) (string, string) {
	command, argument, found := strings.Cut(line, " ")
	if !found {
		return strings.ToUpper(command), ""
	}
	return strings.ToUpper(command), argument
}

func (s *Server) reply(conn net.Conn, writer *bufio.Writer, line string) bool {
	if err := conn.SetWriteDeadline(time.Now().Add(s.idleTimeout)); err != nil {
		return false
	}
	if _, err := writer.WriteString(line + "\r\n"); err != nil {
		return false
	}
	return writer.Flush() == nil
}

func (s *Server) replyMulti(conn net.Conn, writer *bufio.Writer, lines []string) bool {
	for _, line := range lines {
		if _, err := writer.WriteString(line + "\r\n"); err != nil {
			return false
		}
	}
	if _, err := writer.WriteString(".\r\n"); err != nil {
		return false
	}
	return writer.Flush() == nil
}

func clientIP(address net.Addr) string {
	host, _, err := net.SplitHostPort(address.String())
	if err == nil && net.ParseIP(host) != nil {
		return host
	}
	if ip := net.ParseIP(address.String()); ip != nil {
		return ip.String()
	}
	return "unknown"
}
