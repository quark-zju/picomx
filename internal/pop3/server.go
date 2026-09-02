// Package pop3 implements picomx's deliberately read-only POP3 surface.
package pop3

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"picomx/internal/archive"
)

const (
	minimumIdleTimeout = 10 * time.Minute
	maxCommandBytes    = 255
)

type mailbox interface {
	Snapshot() archive.Snapshot
	Size(uint64) (uint64, error)
	Open(uint64) (*os.File, error)
}

// Options configures the POP3 protocol state machine.
type Options struct {
	Hostname        string
	Mailbox         mailbox
	Credentials     Credentials
	IdleTimeout     time.Duration
	MaxAuthFailures int
	MaxConnections  int
	Logger          *slog.Logger
}

// Server serves one read-only maildrop.
type Server struct {
	hostname        string
	mailbox         mailbox
	credentials     Credentials
	idleTimeout     time.Duration
	maxAuthFailures int
	connections     chan struct{}
	logger          *slog.Logger
	wg              sync.WaitGroup
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
	if options.MaxConnections <= 0 {
		options.MaxConnections = 16
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
		connections:     make(chan struct{}, options.MaxConnections),
		logger:          options.Logger,
	}, nil
}

// Serve accepts implicit TLS connections until listener is closed.
func (s *Server) Serve(listener net.Listener, tlsConfig *tls.Config) error {
	if tlsConfig == nil {
		return errors.New("POP3S TLS configuration is required")
	}
	defer s.wg.Wait()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		select {
		case s.connections <- struct{}{}:
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer func() { <-s.connections }()
				s.serveTLSConn(conn, tlsConfig)
			}()
		default:
			_ = conn.Close()
		}
	}
}

func (s *Server) serveTLSConn(conn net.Conn, tlsConfig *tls.Config) {
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(s.idleTimeout)); err != nil {
		return
	}
	tlsConn := tls.Server(conn, tlsConfig.Clone())
	if err := tlsConn.Handshake(); err != nil {
		s.logger.Info("POP3S TLS handshake failed", "client_ip", clientIP(conn.RemoteAddr()), "error", err)
		return
	}
	state := tlsConn.ConnectionState()
	s.serveConn(tlsConn, state.Version)
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
		logAttrs := []any{
			"protocol", "pop3",
			"client_ip", remoteIP,
			"tls_version", tlsVersion,
			"command", command,
		}
		// Never include the PASS argument in logs; it contains the app password.
		if command != "PASS" && argument != "" {
			logAttrs = append(logAttrs, "argument", argument)
		}
		s.logger.Info("POP3 command", logAttrs...)
		switch command {
		case "CAPA":
			if argument != "" {
				s.reply(conn, writer, "-ERR invalid arguments")
				continue
			}
			s.replyMulti(conn, writer, []string{"+OK Capability list follows", "USER", "UIDL", "TOP", "IMPLEMENTATION picomx"})
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
			s.logger.Info("POP3 authentication succeeded",
				"protocol", "pop3",
				"client_ip", remoteIP,
				"tls_version", tlsVersion,
				"username", username,
			)
			s.reply(conn, writer, fmt.Sprintf("+OK maildrop ready with %d messages", snapshot.LastID))
		case "STAT":
			if !authenticated || argument != "" {
				s.reply(conn, writer, "-ERR command not valid now")
				continue
			}
			s.reply(conn, writer, fmt.Sprintf("+OK %d %d", snapshot.LastID, snapshot.TotalOctets))
		case "LIST":
			if !authenticated {
				s.reply(conn, writer, "-ERR authenticate first")
				continue
			}
			if argument != "" {
				id, ok := messageID(argument, snapshot.LastID)
				if !ok {
					s.reply(conn, writer, "-ERR no such message")
					continue
				}
				size, err := s.mailbox.Size(id)
				if err != nil {
					s.reply(conn, writer, "-ERR unable to inspect message")
					continue
				}
				s.reply(conn, writer, fmt.Sprintf("+OK %d %d", id, size))
				continue
			}
			if !s.writeListing(conn, writer, snapshot.LastID, func(id uint64) (string, error) {
				size, err := s.mailbox.Size(id)
				return fmt.Sprintf("%d %d", id, size), err
			}) {
				return
			}
		case "UIDL":
			if !authenticated {
				s.reply(conn, writer, "-ERR authenticate first")
				continue
			}
			if argument != "" {
				id, ok := messageID(argument, snapshot.LastID)
				if !ok {
					s.reply(conn, writer, "-ERR no such message")
					continue
				}
				s.reply(conn, writer, fmt.Sprintf("+OK %d %s", id, uniqueID(id)))
				continue
			}
			if !s.writeListing(conn, writer, snapshot.LastID, func(id uint64) (string, error) {
				return fmt.Sprintf("%d %s", id, uniqueID(id)), nil
			}) {
				return
			}
		case "RETR":
			if !authenticated {
				s.reply(conn, writer, "-ERR authenticate first")
				continue
			}
			id, ok := messageID(argument, snapshot.LastID)
			if !ok {
				s.reply(conn, writer, "-ERR no such message")
				continue
			}
			size, err := s.mailbox.Size(id)
			if err != nil {
				s.reply(conn, writer, "-ERR unable to inspect message")
				continue
			}
			file, err := s.mailbox.Open(id)
			if err != nil {
				s.reply(conn, writer, "-ERR unable to open message")
				continue
			}
			ok = s.streamMessage(conn, writer, file, fmt.Sprintf("+OK %d octets", size), nil)
			_ = file.Close()
			if !ok {
				return
			}
		case "TOP":
			if !authenticated {
				s.reply(conn, writer, "-ERR authenticate first")
				continue
			}
			id, bodyLines, ok := topArguments(argument, snapshot.LastID)
			if !ok {
				s.reply(conn, writer, "-ERR invalid arguments")
				continue
			}
			file, err := s.mailbox.Open(id)
			if err != nil {
				s.reply(conn, writer, "-ERR unable to open message")
				continue
			}
			ok = s.streamMessage(conn, writer, file, "+OK top of message follows", &bodyLines)
			_ = file.Close()
			if !ok {
				return
			}
		case "DELE":
			if !authenticated {
				s.reply(conn, writer, "-ERR authenticate first")
				continue
			}
			if _, ok := messageID(argument, snapshot.LastID); !ok {
				s.reply(conn, writer, "-ERR no such message")
				continue
			}
			s.reply(conn, writer, "-ERR archive is read-only")
		case "RSET":
			if !authenticated || argument != "" {
				s.reply(conn, writer, "-ERR command not valid now")
				continue
			}
			s.reply(conn, writer, "+OK no messages deleted")
		case "NOOP":
			if !authenticated || argument != "" {
				s.reply(conn, writer, "-ERR command not valid now")
				continue
			}
			s.reply(conn, writer, "+OK")
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
	if err := conn.SetWriteDeadline(time.Now().Add(s.idleTimeout)); err != nil {
		return false
	}
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

func (s *Server) writeListing(conn net.Conn, writer *bufio.Writer, lastID uint64, row func(uint64) (string, error)) bool {
	if err := conn.SetWriteDeadline(time.Now().Add(s.idleTimeout)); err != nil {
		return false
	}
	if _, err := writer.WriteString("+OK listing follows\r\n"); err != nil {
		return false
	}
	if lastID > 0 {
		for id := uint64(1); ; id++ {
			line, err := row(id)
			if err != nil {
				return false
			}
			if _, err := writer.WriteString(line + "\r\n"); err != nil {
				return false
			}
			if id == lastID {
				break
			}
		}
	}
	if _, err := writer.WriteString(".\r\n"); err != nil {
		return false
	}
	return writer.Flush() == nil
}

func messageID(argument string, lastID uint64) (uint64, bool) {
	id, err := strconv.ParseUint(strings.TrimSpace(argument), 10, 64)
	return id, err == nil && id > 0 && id <= lastID
}

func uniqueID(id uint64) string {
	return "picomx-" + strconv.FormatUint(id, 36)
}

func topArguments(argument string, lastID uint64) (uint64, uint64, bool) {
	fields := strings.Fields(argument)
	if len(fields) != 2 {
		return 0, 0, false
	}
	id, ok := messageID(fields[0], lastID)
	if !ok {
		return 0, 0, false
	}
	lines, err := strconv.ParseUint(fields[1], 10, 64)
	return id, lines, err == nil
}

func (s *Server) streamMessage(conn net.Conn, writer *bufio.Writer, source io.Reader, greeting string, bodyLimit *uint64) bool {
	if err := conn.SetWriteDeadline(time.Now().Add(s.idleTimeout)); err != nil {
		return false
	}
	if _, err := writer.WriteString(greeting + "\r\n"); err != nil {
		return false
	}
	reader := bufio.NewReader(source)
	inBody := false
	bodyLines := uint64(0)
	lastEndedWithLF := true
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if bodyLimit != nil && inBody && bodyLines >= *bodyLimit {
				break
			}
			if len(line) > 0 && line[0] == '.' {
				if err := writer.WriteByte('.'); err != nil {
					return false
				}
			}
			if _, writeErr := writer.Write(line); writeErr != nil {
				return false
			}
			lastEndedWithLF = line[len(line)-1] == '\n'
			if inBody {
				bodyLines++
			} else if string(line) == "\r\n" {
				inBody = true
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return false
		}
	}
	if !lastEndedWithLF {
		if _, err := writer.WriteString("\r\n"); err != nil {
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
