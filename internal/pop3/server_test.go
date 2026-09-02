package pop3

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

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
