package hub

import (
	"testing"
	"time"
)

func TestAuthStoreSignalExchange(t *testing.T) {
	store := newAuthStore()
	minted, err := store.MintSignal(time.Minute, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if minted.Signal == "" || minted.TenantID == "" {
		t.Fatalf("expected signal and tenant: %+v", minted)
	}
	credential, err := store.Exchange(exchangeRequest{Signal: minted.Signal, Role: "worker", DeviceName: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if credential.Credential == "" || credential.Role != "worker" || credential.TenantID != minted.TenantID {
		t.Fatalf("unexpected credential: %+v", credential)
	}
	if _, ok := store.Credential(credential.Credential); !ok {
		t.Fatal("expected credential to authorize")
	}
	controlCredential, err := store.Exchange(exchangeRequest{Signal: minted.Signal, Role: "control"})
	if err != nil {
		t.Fatal(err)
	}
	if controlCredential.Credential == credential.Credential || controlCredential.Role != "control" {
		t.Fatalf("unexpected control credential: %+v", controlCredential)
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
