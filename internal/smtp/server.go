// Package smtp implements the deliberately small inbound SMTP surface used by
// picomx. It is a final-delivery server, not a relay or submission server.
package smtp

import (
	"bufio"
	"bytes"
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
)

const (
	defaultMaxMessageBytes = int64(25 << 20)
	defaultMaxRecipients   = 20
	defaultMaxConnections  = 64
	defaultIdleTimeout     = 5 * time.Minute
	maxCommandLineBytes    = 512
	maxDataLineBytes       = 1000
)

var (
	errMessageTooLarge = errors.New("message exceeds size limit")
	errDataLineTooLong = errors.New("SMTP DATA line exceeds limit")
)

// Delivery persists one complete RFC 5322 message.
type Delivery interface {
	Deliver(message io.Reader) (string, error)
}

// Options configures an inbound server.
type Options struct {
	Hostname       string
	Domains        []string
	Delivery       Delivery
	TLSConfig      *tls.Config
	MaxMessageSize int64
	MaxRecipients  int
	MaxConnections int
	IdleTimeout    time.Duration
	Logger         *slog.Logger
}

// Server accepts SMTP connections and delivers only to configured domains.
type Server struct {
	hostname       string
	domains        map[string]struct{}
	delivery       Delivery
	tlsConfig      *tls.Config
	maxMessageSize int64
	maxRecipients  int
	idleTimeout    time.Duration
	logger         *slog.Logger
	connections    chan struct{}
	wg             sync.WaitGroup
}

// NewServer validates options and constructs a server.
func NewServer(options Options) (*Server, error) {
	hostname := normalizeDomain(options.Hostname)
	if !validDomain(hostname) {
		return nil, errors.New("valid SMTP hostname is required")
	}
	if options.Delivery == nil {
		return nil, errors.New("delivery store is required")
	}
	domains := make(map[string]struct{}, len(options.Domains))
	for _, raw := range options.Domains {
		domain := normalizeDomain(raw)
		if !validDomain(domain) {
			return nil, fmt.Errorf("invalid recipient domain %q", raw)
		}
		domains[domain] = struct{}{}
	}
	if len(domains) == 0 {
		return nil, errors.New("at least one recipient domain is required")

	}
	if options.MaxMessageSize <= 0 {
		options.MaxMessageSize = defaultMaxMessageBytes
	}
	if options.MaxRecipients <= 0 {
		options.MaxRecipients = defaultMaxRecipients
	}
	if options.MaxConnections <= 0 {
		options.MaxConnections = defaultMaxConnections
	}
	if options.IdleTimeout <= 0 {
		options.IdleTimeout = defaultIdleTimeout
	}
	if options.Logger == nil {
		options.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &Server{
		hostname:       hostname,
		domains:        domains,
		delivery:       options.Delivery,
		tlsConfig:      options.TLSConfig,
		maxMessageSize: options.MaxMessageSize,
		maxRecipients:  options.MaxRecipients,
		idleTimeout:    options.IdleTimeout,
		logger:         options.Logger,
		connections:    make(chan struct{}, options.MaxConnections),
	}, nil
}

// Serve accepts connections until listener is closed, then waits for active
// sessions to finish.
func (s *Server) Serve(listener net.Listener) error {
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
				s.serveConn(conn)
			}()
		default:
			_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
			_, _ = io.WriteString(conn, "421 4.3.2 too many connections\r\n")
			_ = conn.Close()
		}
	}
}

type transaction struct {
	started    bool
	from       string
	recipients []string
}

func (s *Server) serveConn(initialConn net.Conn) {
	conn := initialConn
	defer func() { _ = conn.Close() }()
	remote := remoteIP(conn.RemoteAddr())
	reader := bufio.NewReaderSize(conn, maxDataLineBytes+2)
	writer := bufio.NewWriter(conn)
	greeted := false
	helo := ""
	encrypted := false
	tx := transaction{}

	if !s.reply(conn, writer, 220, s.hostname+" ESMTP picomx") {
		return
	}
	for {
		if err := conn.SetDeadline(time.Now().Add(s.idleTimeout)); err != nil {
			return
		}
		line, err := readLimitedLine(reader, maxCommandLineBytes)
		if err != nil {
			if errors.Is(err, errDataLineTooLong) {
				s.reply(conn, writer, 500, "5.5.2 command line too long")
			}
			return
		}
		command, argument := splitCommand(line)
		switch command {
		case "EHLO", "HELO":
			if !validHelo(argument) {
				s.reply(conn, writer, 501, "5.5.2 invalid hostname")
				continue
			}
			greeted, helo, tx = true, argument, transaction{}
			if command == "EHLO" {
				capabilities := []string{
					s.hostname,
					fmt.Sprintf("SIZE %d", s.maxMessageSize),
					"8BITMIME",
				}
				if s.tlsConfig != nil && !encrypted {
					capabilities = append(capabilities, "STARTTLS")
				}
				s.replyMulti(conn, writer, 250, capabilities)
			} else {
				s.reply(conn, writer, 250, s.hostname)
			}
		case "STARTTLS":
			if argument != "" || s.tlsConfig == nil || encrypted {
				s.reply(conn, writer, 502, "5.5.1 STARTTLS unavailable")
				continue
			}
			if !s.reply(conn, writer, 220, "2.0.0 ready to start TLS") {
				return
			}
			tlsConn := tls.Server(conn, s.tlsConfig.Clone())
			if err := tlsConn.Handshake(); err != nil {
				s.logger.Info("TLS handshake failed", "remote_ip", remote, "error", err)
				return
			}
			conn = tlsConn
			reader = bufio.NewReaderSize(conn, maxDataLineBytes+2)
			writer = bufio.NewWriter(conn)
			greeted, helo, encrypted, tx = false, "", true, transaction{}
		case "MAIL":
			if !greeted {
				s.reply(conn, writer, 503, "5.5.1 send HELO/EHLO first")
				continue
			}
			address, params, err := parsePath(argument, "FROM:", true)
			if err != nil {
				s.reply(conn, writer, 501, "5.5.2 invalid reverse-path")
				continue
			}
			if err := validateMailParameters(params, s.maxMessageSize); err != nil {
				if errors.Is(err, errMessageTooLarge) {
					s.reply(conn, writer, 552, "5.3.4 message exceeds fixed maximum size")
				} else {
					s.reply(conn, writer, 501, "5.5.4 invalid MAIL parameters")
				}
				continue
			}
			tx = transaction{started: true, from: address}
			s.reply(conn, writer, 250, "2.1.0 sender accepted")
		case "RCPT":
			if !tx.started {
				s.reply(conn, writer, 503, "5.5.1 send MAIL first")
				continue
			}
			address, params, err := parsePath(argument, "TO:", false)
			if err != nil || strings.TrimSpace(params) != "" {
				s.reply(conn, writer, 501, "5.5.2 invalid forward-path")
				continue
			}
			_, domain, _ := strings.Cut(address, "@")
			if _, ok := s.domains[normalizeDomain(domain)]; !ok {
				s.reply(conn, writer, 550, "5.1.1 recipient domain not served")
				continue
			}
			if len(tx.recipients) >= s.maxRecipients {
				s.reply(conn, writer, 452, "4.5.3 too many recipients")
				continue
			}
			tx.recipients = append(tx.recipients, address)
			s.reply(conn, writer, 250, "2.1.5 recipient accepted")
		case "DATA":
			if argument != "" || len(tx.recipients) == 0 {
				s.reply(conn, writer, 503, "5.5.1 valid MAIL and RCPT required")
				continue
			}
			if !s.reply(conn, writer, 354, "end with <CRLF>.<CRLF>") {
				return
			}
			headers := s.deliveryHeaders(remote, helo, encrypted, tx)
			data := &dataReader{reader: reader, maxBytes: s.maxMessageSize}
			path, deliverErr := s.delivery.Deliver(io.MultiReader(strings.NewReader(headers), data))
			if deliverErr != nil {
				code, message := 451, "4.3.0 temporary local failure"
				if errors.Is(deliverErr, errMessageTooLarge) {
					code, message = 552, "5.3.4 message exceeds size limit"
				}
				if errors.Is(deliverErr, errDataLineTooLong) {
					code, message = 550, "5.6.0 DATA line exceeds limit"
				}
				s.logger.Warn("message delivery failed", "remote_ip", remote, "error", deliverErr)
				s.reply(conn, writer, code, message)
				return // DATA may not be synchronized after a streaming error.
			}
			s.logger.Info("message delivered",
				"remote_ip", remote,
				"envelope_from", tx.from,
				"recipients", tx.recipients,
				"bytes", data.bytesRead,
				"path", path,
				"tls", encrypted,
			)
			tx = transaction{}
			s.reply(conn, writer, 250, "2.0.0 message accepted")
		case "RSET":
			tx = transaction{}
			s.reply(conn, writer, 250, "2.0.0 reset")
		case "NOOP":
			s.reply(conn, writer, 250, "2.0.0 ok")
		case "QUIT":
			s.reply(conn, writer, 221, "2.0.0 bye")
			return
		default:
			s.reply(conn, writer, 502, "5.5.1 command not implemented")
		}
	}
}

func (s *Server) deliveryHeaders(remote, helo string, encrypted bool, tx transaction) string {
	var headers strings.Builder
	if tx.from == "" {
		headers.WriteString("Return-Path: <>\r\n")
	} else {
		fmt.Fprintf(&headers, "Return-Path: <%s>\r\n", tx.from)
	}
	for _, recipient := range tx.recipients {
		fmt.Fprintf(&headers, "Delivered-To: %s\r\n", recipient)
	}
	fmt.Fprintf(
		&headers,
		"Received: from %s ([%s]) by %s with %sSMTP; %s\r\n",
		helo,
		remote,
		s.hostname,
		map[bool]string{true: "E", false: ""}[encrypted],
		time.Now().UTC().Format(time.RFC1123Z),
	)
	return headers.String()
}

func (s *Server) reply(conn net.Conn, writer *bufio.Writer, code int, message string) bool {
	if err := conn.SetWriteDeadline(time.Now().Add(s.idleTimeout)); err != nil {
		return false
	}
	if _, err := fmt.Fprintf(writer, "%d %s\r\n", code, message); err != nil {
		return false
	}
	return writer.Flush() == nil
}

func (s *Server) replyMulti(conn net.Conn, writer *bufio.Writer, code int, lines []string) bool {
	if err := conn.SetWriteDeadline(time.Now().Add(s.idleTimeout)); err != nil {
		return false
	}
	for i, line := range lines {
		separator := "-"
		if i == len(lines)-1 {
			separator = " "
		}
		if _, err := fmt.Fprintf(writer, "%d%s%s\r\n", code, separator, line); err != nil {
			return false
		}
	}
	return writer.Flush() == nil
}

type dataReader struct {
	reader    *bufio.Reader
	pending   []byte
	done      bool
	maxBytes  int64
	bytesRead int64
}

func (r *dataReader) Read(buffer []byte) (int, error) {
	if len(r.pending) > 0 {
		n := copy(buffer, r.pending)
		r.pending = r.pending[n:]
		return n, nil
	}
	if r.done {
		return 0, io.EOF
	}
	line, err := readLimitedLineRaw(r.reader, maxDataLineBytes)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return 0, io.ErrUnexpectedEOF
		}
		return 0, err
	}
	if bytes.Equal(line, []byte(".\r\n")) || bytes.Equal(line, []byte(".\n")) {
		r.done = true
		return 0, io.EOF
	}
	if len(line) >= 2 && line[0] == '.' && line[1] == '.' {
		line = line[1:]
	}
	if r.bytesRead+int64(len(line)) > r.maxBytes {
		return 0, errMessageTooLarge
	}
	r.bytesRead += int64(len(line))
	r.pending = line
	return r.Read(buffer)
}

func readLimitedLine(reader *bufio.Reader, limit int) (string, error) {
	line, err := readLimitedLineRaw(reader, limit)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r"), nil
}

func readLimitedLineRaw(reader *bufio.Reader, limit int) ([]byte, error) {
	line, err := reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) || len(line) > limit {
		return nil, errDataLineTooLong
	}
	if err != nil {
		return nil, err
	}
	return line, nil
}

func splitCommand(line string) (string, string) {
	command, argument, found := strings.Cut(line, " ")
	if !found {
		return strings.ToUpper(command), ""
	}
	return strings.ToUpper(command), strings.TrimSpace(argument)
}

func parsePath(argument, prefix string, allowEmpty bool) (address, parameters string, err error) {
	if len(argument) < len(prefix) || !strings.EqualFold(argument[:len(prefix)], prefix) {
		return "", "", errors.New("missing path prefix")
	}
	rest := strings.TrimSpace(argument[len(prefix):])
	if !strings.HasPrefix(rest, "<") {
		return "", "", errors.New("missing opening angle bracket")
	}
	end := strings.IndexByte(rest, '>')
	if end < 0 {
		return "", "", errors.New("missing closing angle bracket")
	}
	address = rest[1:end]
	parameters = strings.TrimSpace(rest[end+1:])
	if address == "" && allowEmpty {
		return address, parameters, nil
	}
	if !validAddress(address) {
		return "", "", errors.New("invalid address")
	}
	return address, parameters, nil
}

func validAddress(address string) bool {
	if len(address) > 254 || strings.Count(address, "@") != 1 {
		return false
	}
	local, domain, _ := strings.Cut(address, "@")
	if local == "" || len(local) > 64 || !validDomain(normalizeDomain(domain)) {
		return false
	}
	if strings.HasPrefix(local, ".") || strings.HasSuffix(local, ".") || strings.Contains(local, "..") {
		return false
	}
	for _, char := range local {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || strings.ContainsRune("!#$%&'*+-/=?^_`{|}~.", char) {
			continue
		}
		return false
	}
	return true
}

func validateMailParameters(parameters string, maxSize int64) error {
	if parameters == "" {
		return nil
	}
	for _, parameter := range strings.Fields(parameters) {
		name, value, found := strings.Cut(parameter, "=")
		if !found || !strings.EqualFold(name, "SIZE") {
			return errors.New("unsupported MAIL parameter")
		}
		size, err := strconv.ParseInt(value, 10, 64)
		if err != nil || size < 0 {
			return errors.New("invalid SIZE value")
		}
		if size > maxSize {
			return errMessageTooLarge
		}
	}
	return nil
}

func validDomain(domain string) bool {
	if domain == "" || len(domain) > 253 {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' {
				continue
			}
			return false
		}
	}
	return true
}

func normalizeDomain(domain string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
}

func validHelo(value string) bool {
	if validDomain(normalizeDomain(value)) {
		return true
	}
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		return net.ParseIP(value[1:len(value)-1]) != nil
	}
	return false
}

func remoteIP(address net.Addr) string {
	host, _, err := net.SplitHostPort(address.String())
	if err == nil && net.ParseIP(host) != nil {
		return host
	}
	if ip := net.ParseIP(address.String()); ip != nil {
		return ip.String()
	}
	return "unknown"
}
