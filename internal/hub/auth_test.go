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
	if _, err := store.Exchange(exchangeRequest{Signal: minted.Signal, Role: "control"}); err == nil {
		t.Fatal("expected worker join signal to reject control exchange")
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

func TestSQLiteAuthStorePersistsUsersAndSignals(t *testing.T) {
	path := t.TempDir() + "/agentmux.db"
	store, err := OpenSQLiteAuthStore(path)
	if err != nil {
		t.Fatal(err)
	}
	minted, err := store.MintSignal(time.Hour, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	registered, err := store.Register(registerRequest{
		Email: "persist@example.com", Password: "password123", Name: "Persist",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSQLiteAuthStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	loggedIn, err := reopened.Login(loginRequest{Email: "persist@example.com", Password: "password123"})
	if err != nil {
		t.Fatal(err)
	}
	if loggedIn.TenantID != registered.TenantID {
		t.Fatalf("tenant was not persisted: got %s want %s", loggedIn.TenantID, registered.TenantID)
	}
	credential, err := reopened.Exchange(exchangeRequest{Signal: minted.Signal, Role: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if credential.TenantID != minted.TenantID || credential.Credential == "" {
		t.Fatalf("unexpected persisted signal exchange: %+v", credential)
	}
}

func TestAuthStoreDeviceLogin(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		testAuthStoreDeviceLogin(t, newAuthStore())
	})
	t.Run("sqlite", func(t *testing.T) {
		store, err := OpenSQLiteAuthStore(t.TempDir() + "/agentmux.db")
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
		}()
		testAuthStoreDeviceLogin(t, store)
	})
}

func testAuthStoreDeviceLogin(t *testing.T, store AuthStore) {
	t.Helper()
	registered, err := store.Register(registerRequest{
		Email: "device@example.com", Password: "password123", Name: "Device",
	})
	if err != nil {
		t.Fatal(err)
	}
	start, err := store.StartDeviceAuth(deviceStartRequest{DeviceID: "cli", DeviceName: "CLI"})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := store.PollDeviceAuth(devicePollRequest{DeviceCode: start.DeviceCode})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != "pending" {
		t.Fatalf("expected pending, got %+v", pending)
	}
	approved, err := store.ApproveDeviceAuth(deviceApproveRequest{
		UserCode: start.UserCode, Email: "device@example.com", Password: "password123",
	})
	if err != nil {
		t.Fatal(err)
	}
	if approved.TenantID != registered.TenantID || approved.Role != "control" {
		t.Fatalf("unexpected approved credential: %+v", approved)
	}
	polled, err := store.PollDeviceAuth(devicePollRequest{DeviceCode: start.DeviceCode})
	if err != nil {
		t.Fatal(err)
	}
	if polled.Status != "approved" || polled.Credential == nil || polled.Credential.Credential != approved.Credential {
		t.Fatalf("unexpected poll response: %+v", polled)
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
