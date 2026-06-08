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

func TestAuthStoreRegisterLogin(t *testing.T) {
	store := newAuthStore()
	registered, err := store.Register(registerRequest{
		Email: "User@Example.com", Password: "password123", Name: "User",
		DeviceName: "browser",
	})
	if err != nil {
		t.Fatal(err)
	}
	if registered.Credential == "" || registered.User.Email != "user@example.com" || registered.TenantID == "" {
		t.Fatalf("unexpected registration response: %+v", registered)
	}
	if _, ok := store.Credential(registered.Credential); !ok {
		t.Fatal("expected registered credential to authorize")
	}
	if _, err := store.Register(registerRequest{Email: "user@example.com", Password: "password123"}); err == nil {
		t.Fatal("expected duplicate registration to fail")
	}
	if _, err := store.Login(loginRequest{Email: "user@example.com", Password: "wrong-password"}); err == nil {
		t.Fatal("expected wrong password to fail")
	}
	loggedIn, err := store.Login(loginRequest{Email: "user@example.com", Password: "password123"})
	if err != nil {
		t.Fatal(err)
	}
	if loggedIn.Credential == "" || loggedIn.Credential == registered.Credential || loggedIn.TenantID != registered.TenantID {
		t.Fatalf("unexpected login response: %+v", loggedIn)
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
