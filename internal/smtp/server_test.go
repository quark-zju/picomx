package smtp

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

type memoryDelivery struct {
	mu       sync.Mutex
	messages [][]byte
}

func (d *memoryDelivery) Deliver(message io.Reader) (string, error) {
	content, err := io.ReadAll(message)
	if err != nil {
		return "", err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.messages = append(d.messages, content)
	return fmt.Sprintf("message-%d", len(d.messages)), nil
}

func (d *memoryDelivery) all() [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([][]byte(nil), d.messages...)
}

func TestServerDeliversLocalRecipientAndUnstuffsDots(t *testing.T) {
	t.Parallel()

	delivery := &memoryDelivery{}
	client, responses := startTestSession(t, delivery, 1024)
	expectCode(t, responses, 220)
	writeLine(t, client, "EHLO sender.example")
	expectCode(t, responses, 250)
	expectCode(t, responses, 250)
	expectCode(t, responses, 250)
	writeLine(t, client, "MAIL FROM:<bounce@sender.example> SIZE=50")
	expectCode(t, responses, 250)
	writeLine(t, client, "RCPT TO:<shop-name@mail.example>")
	expectCode(t, responses, 250)
	writeLine(t, client, "DATA")
	expectCode(t, responses, 354)
	writeRaw(t, client, "From: Sender <sender@sender.example>\r\nSubject: sale\r\n\r\n..leading dot\r\n.\r\n")
	expectCode(t, responses, 250)
	writeLine(t, client, "QUIT")
	expectCode(t, responses, 221)
	_ = client.Close()

	messages := delivery.all()
	if len(messages) != 1 {
		t.Fatalf("delivered %d messages, want 1", len(messages))
	}
	message := messages[0]
	for _, expected := range [][]byte{
		[]byte("Return-Path: <bounce@sender.example>\r\n"),
		[]byte("Delivered-To: shop-name@mail.example\r\n"),
		[]byte("Received: from sender.example ([unknown]) by mx.mail.example with SMTP;"),
		[]byte("\r\n.leading dot\r\n"),
	} {
		if !bytes.Contains(message, expected) {
			t.Errorf("message missing %q:\n%s", expected, message)
		}
	}
}

func TestServerRejectsRelayRecipient(t *testing.T) {
	t.Parallel()

	delivery := &memoryDelivery{}
	client, responses := startTestSession(t, delivery, 1024)
	expectCode(t, responses, 220)
	writeLine(t, client, "HELO sender.example")
	expectCode(t, responses, 250)
	writeLine(t, client, "MAIL FROM:<sender@sender.example>")
	expectCode(t, responses, 250)
	writeLine(t, client, "RCPT TO:<victim@gmail.com>")
	expectCode(t, responses, 550)
	writeLine(t, client, "DATA")
	expectCode(t, responses, 503)
	writeLine(t, client, "QUIT")
	expectCode(t, responses, 221)
	_ = client.Close()

	if got := len(delivery.all()); got != 0 {
		t.Fatalf("delivered %d relay messages", got)
	}
}

func TestServerRejectsOversizedStreamingMessage(t *testing.T) {
	t.Parallel()

	delivery := &memoryDelivery{}
	client, responses := startTestSession(t, delivery, 5)
	expectCode(t, responses, 220)
	writeLine(t, client, "HELO sender.example")
	expectCode(t, responses, 250)
	writeLine(t, client, "MAIL FROM:<>")
	expectCode(t, responses, 250)
	writeLine(t, client, "RCPT TO:<offers@mail.example>")
	expectCode(t, responses, 250)
	writeLine(t, client, "DATA")
	expectCode(t, responses, 354)
	writeRaw(t, client, "123456\r\n.\r\n")
	expectCode(t, responses, 552)
	_ = client.Close()

	if got := len(delivery.all()); got != 0 {
		t.Fatalf("delivered %d oversized messages", got)
	}
}

func TestServerRequiresMailBeforeRecipient(t *testing.T) {
	t.Parallel()

	delivery := &memoryDelivery{}
	client, responses := startTestSession(t, delivery, 1024)
	expectCode(t, responses, 220)
	writeLine(t, client, "HELO sender.example")
	expectCode(t, responses, 250)
	writeLine(t, client, "RCPT TO:<offers@mail.example>")
	expectCode(t, responses, 503)
	writeLine(t, client, "QUIT")
	expectCode(t, responses, 221)
	_ = client.Close()
}

func startTestSession(t *testing.T, delivery Delivery, maxSize int64) (net.Conn, *bufio.Reader) {
	t.Helper()
	server, err := NewServer(Options{
		Hostname:       "mx.mail.example",
		Domains:        []string{"mail.example"},
		Delivery:       delivery,
		MaxMessageSize: maxSize,
		IdleTimeout:    5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	serverConn, clientConn := net.Pipe()
	go server.serveConn(serverConn)
	t.Cleanup(func() { _ = clientConn.Close() })
	return clientConn, bufio.NewReader(clientConn)
}

func writeLine(t *testing.T, writer io.Writer, line string) {
	t.Helper()
	writeRaw(t, writer, line+"\r\n")
}

func writeRaw(t *testing.T, writer io.Writer, value string) {
	t.Helper()
	if _, err := io.WriteString(writer, value); err != nil {
		t.Fatal(err)
	}
}

func expectCode(t *testing.T, reader *bufio.Reader, want int) string {
	t.Helper()
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, fmt.Sprintf("%d", want)) {
		t.Fatalf("response = %q, want code %d", line, want)
	}
	return line
}
