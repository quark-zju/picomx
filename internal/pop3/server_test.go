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
	"os/exec"
	"strings"
	"testing"
	"time"

	"picomx/internal/archive"
)

type fakeMailbox struct {
	snapshot        archive.Snapshot
	sizes           map[uint64]uint64
	deleteError     error
	singleSizeError error
}

func (m fakeMailbox) DeleteAs([]uint64, string) error { return m.deleteError }

func (m fakeMailbox) Snapshot() archive.Snapshot {
	snapshot := m.snapshot
	snapshot.MessageCount = snapshot.LastID
	return snapshot
}
func (m fakeMailbox) Lookup(snapshot archive.Snapshot, id uint64) (archive.Message, error) {
	if m.singleSizeError != nil {
		return archive.Message{}, m.singleSizeError
	}
	if id == 0 || id > snapshot.Count() {
		return archive.Message{}, archive.ErrNoSuchMessage
	}
	return archive.Message{ID: id, Size: m.sizes[id]}, nil
}
func (m fakeMailbox) ListAt(snapshot archive.Snapshot, visit func(number, id, size uint64) error) error {
	for id := uint64(1); id <= snapshot.Count(); id++ {
		size, ok := m.sizes[id]
		if !ok {
			return archive.ErrNoSuchMessage
		}
		if err := visit(id, id, size); err != nil {
			return err
		}
	}
	return nil
}
func (m fakeMailbox) OpenMessage(archive.Snapshot, archive.Message) (io.ReadCloser, error) {
	return nil, archive.ErrNoSuchMessage
}

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

	store, err := testArchive(t, t.TempDir())
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

func TestTransactionCommands(t *testing.T) {
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
	expectLine(t, responses, "+OK message marked for deletion")
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
	done := make(chan struct{})
	go func() { defer close(done); server.serveConn(serverConn, 0x0304) }()
	if err := clientConn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = clientConn.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("POP3 session did not close")
		}
	})
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

func TestConcurrentSessionsKeepSnapshotsAndMergeDeletions(t *testing.T) {
	t.Parallel()
	store, err := testArchive(t, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	messages := []string{"Subject: one\r\n\r\none\r\n", "Subject: two\r\n\r\ntwo\r\n", "Subject: three\r\n\r\nthree\r\n"}
	for _, message := range messages {
		if _, err := store.Deliver(strings.NewReader(message)); err != nil {
			t.Fatal(err)
		}
	}
	server, err := NewServer(Options{Hostname: "pop.example.com", Mailbox: store, Credentials: testCredentials(t, "alice", "password")})
	if err != nil {
		t.Fatal(err)
	}
	a, ar := startSession(t, server)
	b, br := startSession(t, server)
	for _, session := range []struct {
		client net.Conn
		reader *bufio.Reader
	}{{a, ar}, {b, br}} {
		expectLine(t, session.reader, "+OK pop.example.com picomx ready")
		login(t, session.client, session.reader, "alice", "password")
	}
	writeLine(t, a, "DELE 2")
	expectLine(t, ar, "+OK message marked for deletion")
	writeLine(t, a, "STAT")
	expectLine(t, ar, fmt.Sprintf("+OK 2 %d", len(messages[0])+len(messages[2])))
	writeLine(t, a, "LIST")
	expectLines(t, ar, "+OK listing follows", fmt.Sprintf("1 %d", len(messages[0])), fmt.Sprintf("3 %d", len(messages[2])), ".")
	writeLine(t, a, "UIDL")
	expectLines(t, ar, "+OK listing follows", "1 picomx-1", "3 picomx-3", ".")
	for _, command := range []string{"DELE 2", "LIST 2", "UIDL 2", "RETR 2"} {
		writeLine(t, a, command)
		expectLine(t, ar, "-ERR no such message")
	}
	writeLine(t, a, "RSET")
	expectLine(t, ar, "+OK no messages deleted")
	writeLine(t, a, "STAT")
	expectLine(t, ar, fmt.Sprintf("+OK 3 %d", len(messages[0])+len(messages[1])+len(messages[2])))
	writeLine(t, a, "DELE 2")
	expectLine(t, ar, "+OK message marked for deletion")
	writeLine(t, a, "QUIT")
	expectLine(t, ar, "+OK bye")

	// B retains the original numbering, sizes and content after A's removal.
	writeLine(t, b, "LIST")
	expectLines(t, br, "+OK listing follows", fmt.Sprintf("1 %d", len(messages[0])), fmt.Sprintf("2 %d", len(messages[1])), fmt.Sprintf("3 %d", len(messages[2])), ".")
	writeLine(t, b, "STAT")
	expectLine(t, br, fmt.Sprintf("+OK 3 %d", len(messages[0])+len(messages[1])+len(messages[2])))
	writeLine(t, b, "RETR 2")
	expectLines(t, br, fmt.Sprintf("+OK %d octets", len(messages[1])), "Subject: two", "", "two", ".")
	writeLine(t, b, "TOP 2 0")
	expectLines(t, br, "+OK top of message follows", "Subject: two", "", ".")
	for _, number := range []string{"2", "3"} {
		writeLine(t, b, "DELE "+number)
		expectLine(t, br, "+OK message marked for deletion")
	}
	if _, err := store.Deliver(strings.NewReader("Subject: four\r\n\r\nfour\r\n")); err != nil {
		t.Fatal(err)
	}
	writeLine(t, b, "UIDL 4")
	expectLine(t, br, "-ERR no such message")
	writeLine(t, b, "QUIT")
	expectLine(t, br, "+OK bye")

	// A fresh session has consecutive message numbers but permanent UIDLs.
	c, cr := startSession(t, server)
	expectLine(t, cr, "+OK pop.example.com picomx ready")
	login(t, c, cr, "alice", "password")
	writeLine(t, c, "UIDL")
	expectLines(t, cr, "+OK listing follows", "1 picomx-1", "2 picomx-4", ".")
	writeLine(t, c, "RETR 2")
	expectLines(t, cr, fmt.Sprintf("+OK %d octets", len("Subject: four\r\n\r\nfour\r\n")), "Subject: four", "", "four", ".")
	writeLine(t, c, "QUIT")
	expectLine(t, cr, "+OK bye")
	if store.Snapshot().Count() != 2 {
		t.Fatalf("mail count = %d", store.Snapshot().Count())
	}
}

func TestDisconnectCancelsPendingDeletions(t *testing.T) {
	t.Parallel()
	store, err := testArchive(t, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Deliver(strings.NewReader("Subject: keep\r\n\r\nbody\r\n")); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(Options{Hostname: "pop.example.com", Mailbox: store, Credentials: testCredentials(t, "alice", "password")})
	if err != nil {
		t.Fatal(err)
	}
	serverConn, client := net.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); server.serveConn(serverConn, 0x0304) }()
	responses := bufio.NewReader(client)
	expectLine(t, responses, "+OK pop.example.com picomx ready")
	login(t, client, responses, "alice", "password")
	writeLine(t, client, "DELE 1")
	expectLine(t, responses, "+OK message marked for deletion")
	_ = client.Close()
	<-done
	if store.Snapshot().Count() != 1 {
		t.Fatal("disconnect committed a deletion")
	}
	if file, err := store.Open(1); err != nil {
		t.Fatal(err)
	} else {
		_ = file.Close()
	}
}

func TestQuitReportsDeletionFailure(t *testing.T) {
	t.Parallel()
	server, err := NewServer(Options{
		Hostname: "pop.example.com", Credentials: testCredentials(t, "alice", "password"),
		Mailbox: fakeMailbox{snapshot: archive.Snapshot{LastID: 1, TotalOctets: 10}, sizes: map[uint64]uint64{1: 10}, deleteError: errors.New("disk unavailable")},
	})
	if err != nil {
		t.Fatal(err)
	}
	client, responses := startSession(t, server)
	expectLine(t, responses, "+OK pop.example.com picomx ready")
	login(t, client, responses, "alice", "password")
	writeLine(t, client, "DELE 1")
	expectLine(t, responses, "+OK message marked for deletion")
	writeLine(t, client, "QUIT")
	expectLine(t, responses, "-ERR unable to commit deletions")
	if _, err := responses.ReadString('\n'); err == nil {
		t.Fatal("QUIT failure left connection open")
	}
}

func testArchive(t *testing.T, root string) (*archive.Store, error) {
	t.Helper()
	store, err := archive.New(root)
	if err == nil {
		t.Cleanup(func() { _ = store.Close() })
	}
	return store, err
}

func TestFullListUsesBatchMetadata(t *testing.T) {
	t.Parallel()
	server, err := NewServer(Options{
		Hostname: "pop.example.com", Credentials: testCredentials(t, "alice", "password"),
		Mailbox: fakeMailbox{snapshot: archive.Snapshot{LastID: 2, TotalOctets: 30}, sizes: map[uint64]uint64{1: 10, 2: 20}, singleSizeError: errors.New("unexpected individual size query")},
	})
	if err != nil {
		t.Fatal(err)
	}
	client, responses := startSession(t, server)
	expectLine(t, responses, "+OK pop.example.com picomx ready")
	login(t, client, responses, "alice", "password")
	writeLine(t, client, "LIST")
	expectLines(t, responses, "+OK listing follows", "1 10", "2 20", ".")
	writeLine(t, client, "QUIT")
	expectLine(t, responses, "+OK bye")
}

func TestMalformedPasswordCommandsDoNotLeakIntoLogs(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	server, err := NewServer(Options{Hostname: "pop.example.com", Mailbox: fakeMailbox{}, Credentials: testCredentials(t, "alice", "super-secret"), Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	client, responses := startSession(t, server)
	expectLine(t, responses, "+OK pop.example.com picomx ready")
	for _, command := range []string{"PASS\tsuper-secret", "PASS\vsuper-secret", "PASS\u00a0super-secret", "PASS\rsuper-secret", "PASS\tsuper-secret extra-secret"} {
		writeLine(t, client, command)
		expectLine(t, responses, "-ERR command not implemented")
	}
	login(t, client, responses, "alice", "super-secret")
	writeLine(t, client, "QUIT")
	expectLine(t, responses, "+OK bye")
	output := strings.ToLower(logs.String())
	if strings.Contains(output, "super-secret") || strings.Contains(output, "extra-secret") {
		t.Fatalf("malformed command leaked password: %s", output)
	}
	if !strings.Contains(output, `"command":"unknown"`) {
		t.Fatal("unknown commands were not logged safely")
	}
}

func TestDeletionCommitUsesAuthenticatedSessionUsername(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := testArchive(t, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Deliver(strings.NewReader("Subject: source\r\nUser-Agent: SenderClient\r\n\r\nbody\r\n")); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(Options{Hostname: "pop.example.com", Mailbox: store, Credentials: testCredentials(t, "alice", "super-secret")})
	if err != nil {
		t.Fatal(err)
	}
	client, responses := startSession(t, server)
	expectLine(t, responses, "+OK pop.example.com picomx ready")
	login(t, client, responses, "alice", "super-secret")
	writeLine(t, client, "DELE 1")
	expectLine(t, responses, "+OK message marked for deletion")
	writeLine(t, client, "QUIT")
	expectLine(t, responses, "+OK bye")
	raw, err := exec.Command("git", "--git-dir="+root, "log", "-1", "--format=%B").Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "mail: delete 1 messages\n\nPOP3 user: alice\n" {
		t.Fatalf("deletion commit = %q", raw)
	}
	if bytes.Contains(raw, []byte("super-secret")) || bytes.Contains(raw, []byte("SenderClient")) {
		t.Fatalf("commit mixed password or sender agent with session actor: %q", raw)
	}
}
