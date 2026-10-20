// Package pop3 implements picomx's POP3 surface with Git-backed deletion.
package pop3

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
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
	ListAt(archive.Snapshot, func(number, id, size uint64) error) error
	Lookup(archive.Snapshot, uint64) (archive.Message, error)
	OpenMessage(archive.Snapshot, archive.Message) (io.ReadCloser, error)
	DeleteAs([]uint64, string) error
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

// Server serves one maildrop with independent, immutable session snapshots.
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
	deleted := map[uint64]uint64{}
	var deletedOctets uint64
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
			"command", loggedCommand(command),
		}
		// Never include the PASS argument in logs; it contains the app password.
		if command != "PASS" && loggedCommand(command) != "UNKNOWN" && argument != "" {
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
			s.reply(conn, writer, fmt.Sprintf("+OK maildrop ready with %d messages", snapshot.Count()))
		case "STAT":
			if !authenticated || argument != "" {
				s.reply(conn, writer, "-ERR command not valid now")
				continue
			}
			s.reply(conn, writer, fmt.Sprintf("+OK %d %d", snapshot.Count()-uint64(len(deleted)), snapshot.TotalOctets-deletedOctets))
		case "LIST":
			if !authenticated {
				s.reply(conn, writer, "-ERR authenticate first")
				continue
			}
			if argument != "" {
				id, ok := messageID(argument, snapshot.Count())
				if !ok || deleted[id] != 0 {
					s.reply(conn, writer, "-ERR no such message")
					continue
				}
				size, err := s.messageSize(snapshot, id)
				if err != nil {
					s.reply(conn, writer, "-ERR unable to inspect message")
					continue
				}
				s.reply(conn, writer, fmt.Sprintf("+OK %d %d", id, size))
				continue
			}
			if !s.writeListing(conn, writer, func(row func(string) error) error {
				return s.mailbox.ListAt(snapshot, func(number, id, size uint64) error {
					if deleted[number] != 0 {
						return nil
					}
					return row(fmt.Sprintf("%d %d", number, size))
				})
			}) {
				return
			}

		case "UIDL":
			if !authenticated {
				s.reply(conn, writer, "-ERR authenticate first")
				continue
			}
			if argument != "" {
				id, ok := messageID(argument, snapshot.Count())
				if !ok || deleted[id] != 0 {
					s.reply(conn, writer, "-ERR no such message")
					continue
				}
				message, err := s.mailbox.Lookup(snapshot, id)
				if err != nil {
					s.reply(conn, writer, "-ERR unable to inspect message")
					continue
				}
				s.reply(conn, writer, fmt.Sprintf("+OK %d %s", id, uniqueID(message.ID)))
				continue
			}
			if !s.writeListing(conn, writer, func(row func(string) error) error {
				return s.mailbox.ListAt(snapshot, func(number, id, size uint64) error {
					if deleted[number] != 0 {
						return nil
					}
					return row(fmt.Sprintf("%d %s", number, uniqueID(id)))
				})
			}) {
				return
			}

		case "RETR":
			if !authenticated {
				s.reply(conn, writer, "-ERR authenticate first")
				continue
			}
			id, ok := messageID(argument, snapshot.Count())
			if !ok || deleted[id] != 0 {
				s.reply(conn, writer, "-ERR no such message")
				continue
			}
			message, err := s.mailbox.Lookup(snapshot, id)
			if err != nil {
				s.reply(conn, writer, "-ERR unable to inspect message")
				continue
			}
			file, err := s.mailbox.OpenMessage(snapshot, message)
			if err != nil {
				s.reply(conn, writer, "-ERR unable to open message")
				continue
			}
			ok = s.streamMessage(conn, writer, file, fmt.Sprintf("+OK %d octets", message.Size), nil)
			_ = file.Close()
			if !ok {
				return
			}
		case "TOP":
			if !authenticated {
				s.reply(conn, writer, "-ERR authenticate first")
				continue
			}
			id, bodyLines, ok := topArguments(argument, snapshot.Count())
			if !ok || deleted[id] != 0 {
				s.reply(conn, writer, "-ERR invalid arguments")
				continue
			}
			file, err := s.openMessage(snapshot, id)
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
			id, ok := messageID(argument, snapshot.Count())
			if !ok || deleted[id] != 0 {
				s.reply(conn, writer, "-ERR no such message")
				continue
			}
			message, err := s.mailbox.Lookup(snapshot, id)
			if err != nil {
				s.reply(conn, writer, "-ERR unable to inspect message")
				continue
			}
			deleted[id] = message.ID
			deletedOctets += message.Size
			s.reply(conn, writer, "+OK message marked for deletion")
		case "RSET":
			if !authenticated || argument != "" {
				s.reply(conn, writer, "-ERR command not valid now")
				continue
			}
			deleted = map[uint64]uint64{}
			deletedOctets = 0
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
			if authenticated && len(deleted) > 0 {
				ids := make([]uint64, 0, len(deleted))
				for _, permanentID := range deleted {
					ids = append(ids, permanentID)
				}
				if err := s.mailbox.DeleteAs(ids, username); err != nil {
					s.logger.Error("POP3 deletion failed", "protocol", "pop3", "client_ip", remoteIP, "error", err)
					s.reply(conn, writer, "-ERR unable to commit deletions")
					return
				}
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

// Unknown tokens may contain a malformed PASS and its password. Only emit
// canonical protocol verbs; never echo unrecognized command text to the log.
func loggedCommand(command string) string {
	switch command {
	case "CAPA", "USER", "PASS", "STAT", "LIST", "UIDL", "RETR", "TOP", "DELE", "RSET", "NOOP", "QUIT":
		return command
	default:
		return "UNKNOWN"
	}
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

func (s *Server) writeListing(conn net.Conn, writer *bufio.Writer, visit func(func(string) error) error) bool {
	if err := conn.SetWriteDeadline(time.Now().Add(s.idleTimeout)); err != nil {
		return false
	}
	if _, err := writer.WriteString("+OK listing follows\r\n"); err != nil {
		return false
	}
	err := visit(func(line string) error {
		_, err := writer.WriteString(line + "\r\n")
		return err
	})
	if err != nil {
		return false
	}
	if _, err := writer.WriteString(".\r\n"); err != nil {
		return false
	}
	return writer.Flush() == nil
}

func (s *Server) messageSize(snapshot archive.Snapshot, number uint64) (uint64, error) {
	message, err := s.mailbox.Lookup(snapshot, number)
	return message.Size, err
}

func (s *Server) openMessage(snapshot archive.Snapshot, number uint64) (io.ReadCloser, error) {
	message, err := s.mailbox.Lookup(snapshot, number)
	if err != nil {
		return nil, err
	}
	return s.mailbox.OpenMessage(snapshot, message)
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
