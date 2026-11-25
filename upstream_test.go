package main

import (
	"bytes"
	"io"
	"net"
	"testing"
)

func TestHandshakeSOCKS5OffersOnlyPasswordAuthWhenCredentialsSet(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()

	errCh := make(chan error, 1)
	go func() {
		conn, _, err := handshakeSOCKS5(client, upstreamConfig{user: "u", pass: "p"}, 0x03, "example.com", 443)
		if conn != nil {
			conn.Close()
		}
		errCh <- err
	}()

	greeting := make([]byte, 3)
	if _, err := io.ReadFull(server, greeting); err != nil {
		t.Fatalf("read greeting: %v", err)
	}
	if !bytes.Equal(greeting, []byte{0x05, 0x01, 0x02}) {
		t.Fatalf("greeting = %v, want [5 1 2]", greeting)
	}
	if _, err := server.Write([]byte{0x05, 0x02}); err != nil {
		t.Fatalf("write auth choice: %v", err)
	}

	auth := make([]byte, 5)
	if _, err := io.ReadFull(server, auth); err != nil {
		t.Fatalf("read auth: %v", err)
	}
	if !bytes.Equal(auth, []byte{0x01, 0x01, 'u', 0x01, 'p'}) {
		t.Fatalf("auth = %v", auth)
	}
	if _, err := server.Write([]byte{0x01, 0x00}); err != nil {
		t.Fatalf("write auth ok: %v", err)
	}

	req := make([]byte, 18)
	if _, err := io.ReadFull(server, req); err != nil {
		t.Fatalf("read connect request: %v", err)
	}
	if _, err := server.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		t.Fatalf("write connect reply: %v", err)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("handshake error: %v", err)
	}
}
