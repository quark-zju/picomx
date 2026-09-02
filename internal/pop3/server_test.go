package pop3

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"picomx/internal/archive"
)

type fakeMailbox struct {
	snapshot archive.Snapshot
	sizes    map[uint64]uint64
}

func (m fakeMailbox) Snapshot() archive.Snapshot { return m.snapshot }
func (m fakeMailbox) Size(id uint64) (uint64, error) {
	size, ok := m.sizes[id]
	if !ok {
		return 0, archive.ErrNoSuchMessage
	}
	return size, nil
}
func (m fakeMailbox) Open(uint64) (*os.File, error) { return nil, archive.ErrNoSuchMessage }

func TestAuthorizationCapturesSnapshot(t *testing.T) {
	t.Parallel()

	credentials := testCredentials(t, "alice", "random-app-password")
	server, err := NewServer(Options{
		Hostname: "pop.example.com",
		Mailbox: fakeMailbox{
			snapshot: archive.Snapshot{LastID: 7, TotalOctets: 99},
			sizes:    map[uint64]uint64{1: 10},
		},
		Credentials: credentials,
	})
	if err != nil {
		t.Fatal(err)
	}
	client, responses := startSession(t, server)
	expectLine(t, responses, "+OK pop.example.com picomx ready")
	writeLine(t, client, "USER alice")
	expectLine(t, responses, "+OK send PASS")
	writeLine(t, client, "PASS random-app-password")
	expectLine(t, responses, "+OK maildrop ready with 7 messages")
	writeLine(t, client, "QUIT")
	expectLine(t, responses, "+OK bye")
}

func TestLogsSuccessfulAuthenticationAndCommandsWithoutPassword(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	server, err := NewServer(Options{
		Hostname:    "pop.example.com",
		Mailbox:     fakeMailbox{},
		Credentials: testCredentials(t, "alice", "super-secret"),
		Logger:      slog.New(slog.NewJSONHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	client, responses := startSession(t, server)
	expectLine(t, responses, "+OK pop.example.com picomx ready")
	login(t, client, responses, "alice", "super-secret")
	writeLine(t, client, "STAT")
	expectLine(t, responses, "+OK 0 0")
	writeLine(t, client, "QUIT")
	expectLine(t, responses, "+OK bye")

	output := logs.String()
	if !strings.Contains(output, `"msg":"POP3 authentication succeeded"`) {
		t.Fatalf("logs do not contain successful authentication: %s", output)
	}
	if !strings.Contains(output, `"command":"STAT"`) {
		t.Fatalf("logs do not contain POP3 command: %s", output)
	}
	if strings.Contains(output, "super-secret") {
		t.Fatalf("logs contain the POP3 password: %s", output)
	}
}

func TestMailboxListingsUseAuthenticatedSnapshot(t *testing.T) {
	t.Parallel()

	mailbox := fakeMailbox{
		snapshot: archive.Snapshot{LastID: 2, TotalOctets: 30},
		sizes:    map[uint64]uint64{1: 10, 2: 20, 3: 30},
	}
	server, err := NewServer(Options{
		Hostname:    "pop.example.com",
		Mailbox:     mailbox,
		Credentials: testCredentials(t, "alice", "password"),
	})
	if err != nil {
		t.Fatal(err)
	}
	client, responses := startSession(t, server)
	expectLine(t, responses, "+OK pop.example.com picomx ready")
	login(t, client, responses, "alice", "password")
	writeLine(t, client, "STAT")
	expectLine(t, responses, "+OK 2 30")
	writeLine(t, client, "LIST 2")
	expectLine(t, responses, "+OK 2 20")
	writeLine(t, client, "LIST")
	expectLines(t, responses, "+OK listing follows", "1 10", "2 20", ".")
	writeLine(t, client, "UIDL 1")
	expectLine(t, responses, "+OK 1 picomx-1")
	writeLine(t, client, "UIDL")
	expectLines(t, responses, "+OK listing follows", "1 picomx-1", "2 picomx-2", ".")
	writeLine(t, client, "LIST 3")
	expectLine(t, responses, "-ERR no such message")
}

func TestRetrieveAndTopStreamCanonicalMessage(t *testing.T) {
	t.Parallel()

	store, err := archive.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	message := "Subject: test\r\nX-Test: yes\r\n\r\nfirst\r\n.second\r\nthird\r\n"
	if _, err := store.Deliver(strings.NewReader(message)); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(Options{
		Hostname:    "pop.example.com",
		Mailbox:     store,
		Credentials: testCredentials(t, "alice", "password"),
	})
	if err != nil {
		t.Fatal(err)
	}
	client, responses := startSession(t, server)
	expectLine(t, responses, "+OK pop.example.com picomx ready")
	login(t, client, responses, "alice", "password")
	writeLine(t, client, "TOP 1 1")
	expectLines(t, responses,
		"+OK top of message follows",
		"Subject: test",
		"X-Test: yes",
		"",
		"first",
		".",
	)
	writeLine(t, client, "RETR 1")
	expectLines(t, responses,
		fmt.Sprintf("+OK %d octets", len(message)),
		"Subject: test",
		"X-Test: yes",
		"",
		"first",
		"..second",
		"third",
		".",
	)
}

func TestReadOnlyTransactionCommands(t *testing.T) {
	t.Parallel()

	mailbox := fakeMailbox{
		snapshot: archive.Snapshot{LastID: 1, TotalOctets: 10},
		sizes:    map[uint64]uint64{1: 10},
	}
	server, err := NewServer(Options{
		Hostname:    "pop.example.com",
		Mailbox:     mailbox,
		Credentials: testCredentials(t, "alice", "password"),
	})
	if err != nil {
		t.Fatal(err)
	}
	client, responses := startSession(t, server)
	expectLine(t, responses, "+OK pop.example.com picomx ready")
	writeLine(t, client, "DELE 1")
	expectLine(t, responses, "-ERR authenticate first")
	login(t, client, responses, "alice", "password")
	writeLine(t, client, "DELE 1")
	expectLine(t, responses, "-ERR archive is read-only")
	writeLine(t, client, "RSET")
	expectLine(t, responses, "+OK no messages deleted")
	writeLine(t, client, "NOOP")
	expectLine(t, responses, "+OK")
	writeLine(t, client, "QUIT")
	expectLine(t, responses, "+OK bye")
}

func TestServerRejectsShortIdleTimeout(t *testing.T) {
	t.Parallel()

	if _, err := NewServer(Options{
		Hostname:    "pop.example.com",
		Mailbox:     fakeMailbox{},
		IdleTimeout: minimumIdleTimeout - time.Second,
	}); err == nil {
		t.Fatal("NewServer accepted POP3 idle timeout below RFC minimum")
	}
}

func TestServeRequiresImplicitTLS(t *testing.T) {
	t.Parallel()

	server, err := NewServer(Options{Hostname: "pop.example.com", Mailbox: fakeMailbox{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Serve(nil, nil); err == nil {
		t.Fatal("Serve accepted a nil TLS configuration")
	}
}

func TestServeNegotiatesTLSBeforeGreeting(t *testing.T) {
	t.Parallel()

	fixture := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	serverTLS := fixture.TLS.Clone()
	fixture.Close()

	server, err := NewServer(Options{Hostname: "pop.example.com", Mailbox: fakeMailbox{}})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve(listener, serverTLS) }()

	client, err := tls.Dial("tcp", listener.Addr().String(), &tls.Config{
		InsecureSkipVerify: true, // The fixture is a generated test certificate.
		MinVersion:         tls.VersionTLS12,
	})
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	responses := bufio.NewReader(client)
	expectLine(t, responses, "+OK pop.example.com picomx ready")
	writeLine(t, client, "QUIT")
	expectLine(t, responses, "+OK bye")
	_ = client.Close()
	_ = listener.Close()
	if err := <-errCh; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Serve returned %v, want net.ErrClosed", err)
	}
}

func TestAuthorizationFailsClosedAndLimitsAttempts(t *testing.T) {
	t.Parallel()

	server, err := NewServer(Options{
		Hostname:        "pop.example.com",
		Mailbox:         fakeMailbox{},
		MaxAuthFailures: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	client, responses := startSession(t, server)
	expectLine(t, responses, "+OK pop.example.com picomx ready")
	for attempt := 0; attempt < 2; attempt++ {
		writeLine(t, client, "USER unknown")
		expectLine(t, responses, "+OK send PASS")
		writeLine(t, client, "PASS wrong")
		expectLine(t, responses, "-ERR authentication failed")
	}
	if _, err := responses.ReadString('\n'); err == nil {
		t.Fatal("connection stayed open after maximum authentication failures")
	}
}

func TestCredentialsRequireBothFields(t *testing.T) {
	t.Parallel()

	for _, fields := range [][2]string{{}, {"alice", ""}, {"", strings.Repeat("0", 64)}} {
		credentials, err := NewCredentials(fields[0], fields[1])
		if err != nil {
			t.Fatal(err)
		}
		if credentials.authenticate("alice", "anything") {
			t.Fatalf("credentials %+v authenticated", fields)
		}
	}
}

func startSession(t *testing.T, server *Server) (net.Conn, *bufio.Reader) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	go server.serveConn(serverConn, 0x0304)
	t.Cleanup(func() { _ = clientConn.Close() })
	return clientConn, bufio.NewReader(clientConn)
}

func testCredentials(t *testing.T, username, password string) Credentials {
	t.Helper()
	digest := sha256.Sum256([]byte(password))
	credentials, err := NewCredentials(username, hex.EncodeToString(digest[:]))
	if err != nil {
		t.Fatal(err)
	}
	return credentials
}

func login(t *testing.T, client io.Writer, responses *bufio.Reader, username, password string) {
	t.Helper()
	writeLine(t, client, "USER "+username)
	expectLine(t, responses, "+OK send PASS")
	writeLine(t, client, "PASS "+password)
	line, err := responses.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "+OK maildrop ready") {
		t.Fatalf("login response = %q", line)
	}
}

func writeLine(t *testing.T, writer io.Writer, line string) {
	t.Helper()
	if _, err := fmt.Fprintf(writer, "%s\r\n", line); err != nil {
		t.Fatal(err)
	}
}

func expectLine(t *testing.T, reader *bufio.Reader, want string) {
	t.Helper()
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"); got != want {
		t.Fatalf("response = %q, want %q", got, want)
	}
}

func expectLines(t *testing.T, reader *bufio.Reader, want ...string) {
	t.Helper()
	for _, line := range want {
		expectLine(t, reader, line)
	}
}
