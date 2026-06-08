package hub

import (
	"testing"
	"time"
)

func TestJoinTokenStoreMintAndConsume(t *testing.T) {
	store := newJoinTokenStore()
	minted, err := store.Mint(time.Minute, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if minted.Token == "" {
		t.Fatal("expected token")
	}
	if !store.Valid(minted.Token) {
		t.Fatal("expected token to be valid")
	}
	if store.Valid(minted.Token) {
		t.Fatal("expected one-use token to be consumed")
	}
}

func TestWebSocketBase(t *testing.T) {
	if got := websocketBase("https://hub.example.com"); got != "wss://hub.example.com" {
		t.Fatalf("unexpected wss base: %q", got)
	}
	if got := websocketBase("http://127.0.0.1:8081"); got != "ws://127.0.0.1:8081" {
		t.Fatalf("unexpected ws base: %q", got)
	}
}
