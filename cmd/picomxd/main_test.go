package main

import (
	"errors"
	"net"
	"testing"
)

type addressOnlyListener struct{ address net.Addr }

func (l addressOnlyListener) Accept() (net.Conn, error) { return nil, errors.New("not implemented") }
func (l addressOnlyListener) Close() error              { return nil }
func (l addressOnlyListener) Addr() net.Addr            { return l.address }

func TestProtocolForActivatedListener(t *testing.T) {
	t.Parallel()

	tests := []struct {
		port int
		want protocol
	}{
		{port: 25, want: protocolSMTP},
		{port: 995, want: protocolPOP3S},
	}
	for _, test := range tests {
		listener := addressOnlyListener{address: &net.TCPAddr{Port: test.port}}
		got, err := protocolForListener(listener)
		if err != nil {
			t.Fatal(err)
		}
		if got != test.want {
			t.Errorf("port %d protocol = %q, want %q", test.port, got, test.want)
		}
	}
}

func TestProtocolForActivatedListenerRejectsUnknownPort(t *testing.T) {
	t.Parallel()

	listener := addressOnlyListener{address: &net.TCPAddr{Port: 443}}
	if _, err := protocolForListener(listener); err == nil {
		t.Fatal("protocolForListener accepted port 443")
	}
}
