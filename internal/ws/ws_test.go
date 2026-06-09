package ws

import (
	"bufio"
	"context"
	"net"
	"net/url"
	"testing"
	"time"
)

func TestWebSocketTextRoundTrip(t *testing.T) {
	serverSide, clientSide := net.Pipe()
	defer serverSide.Close()
	defer clientSide.Close()

	server := &Conn{conn: serverSide, br: bufio.NewReader(serverSide)}
	client := &Conn{conn: clientSide, br: bufio.NewReader(clientSide), writeMask: true}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	errc := make(chan error, 1)
	go func() {
		text, err := server.ReadText()
		if err != nil {
			errc <- err
			return
		}
		errc <- server.WriteText("echo:" + text)
	}()

	go func() {
		errc <- client.WriteText("hello")
	}()

	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	}

	gotc := make(chan string, 1)
	go func() {
		got, err := client.ReadText()
		if err != nil {
			errc <- err
			return
		}
		gotc <- got
	}()

	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case got := <-gotc:
		if got != "echo:hello" {
			t.Fatalf("unexpected echo: %q", got)
		}
	}
}

func TestDialHostDefaultPorts(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{raw: "ws://hub.example.com/ws/worker", want: "hub.example.com:80"},
		{raw: "wss://hub.example.com/ws/worker", want: "hub.example.com:443"},
		{raw: "wss://hub.example.com:8443/ws/worker", want: "hub.example.com:8443"},
		{raw: "wss://[::1]/ws/worker", want: "[::1]:443"},
	}
	for _, tt := range tests {
		parsed, err := url.Parse(tt.raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := dialHost(parsed); got != tt.want {
			t.Fatalf("%s:\n got: %s\nwant: %s", tt.raw, got, tt.want)
		}
	}
}
