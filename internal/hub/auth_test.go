package hub

import (
	"strings"
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

func TestAuthStoreRejectsRepeatedSignalUseByInstance(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		testAuthStoreRejectsRepeatedSignalUseByInstance(t, newAuthStore())
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
		testAuthStoreRejectsRepeatedSignalUseByInstance(t, store)
	})
}

func testAuthStoreRejectsRepeatedSignalUseByInstance(t *testing.T, store AuthStore) {
	t.Helper()
	minted, err := store.MintSignal(time.Minute, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Exchange(exchangeRequest{Signal: minted.Signal, Role: "worker", DeviceID: "one", DeviceName: "one", InstanceID: "instance-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Exchange(exchangeRequest{Signal: minted.Signal, Role: "worker", DeviceID: "renamed", DeviceName: "renamed", InstanceID: "instance-1"}); err == nil {
		t.Fatal("expected same worker instance to be rejected on repeated signal use")
	}
	if _, err := store.Exchange(exchangeRequest{Signal: minted.Signal, Role: "worker", DeviceID: "two", DeviceName: "two", InstanceID: "instance-2"}); err != nil {
		t.Fatalf("different worker instance should still be allowed: %v", err)
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
	if registered.RefreshToken == "" || registered.RefreshExpiresAt.IsZero() {
		t.Fatalf("expected refresh token on registration: %+v", registered)
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
	if loggedIn.RefreshToken == "" || loggedIn.RefreshToken == registered.RefreshToken {
		t.Fatalf("expected rotated login refresh token: %+v", loggedIn)
	}
}

func TestAuthStoreRefreshRotatesToken(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		testAuthStoreRefreshRotatesToken(t, newAuthStore())
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
		testAuthStoreRefreshRotatesToken(t, store)
	})
}

func testAuthStoreRefreshRotatesToken(t *testing.T, store AuthStore) {
	t.Helper()
	registered, err := store.Register(registerRequest{
		Email: "refresh@example.com", Password: "password123", Name: "Refresh",
		DeviceID: "browser", DeviceName: "Browser",
	})
	if err != nil {
		t.Fatal(err)
	}
	if registered.RefreshToken == "" {
		t.Fatal("expected refresh token")
	}
	if time.Until(registered.ExpiresAt) > defaultControlAccessTTL+time.Minute {
		t.Fatalf("access token ttl is too long: %s", time.Until(registered.ExpiresAt))
	}
	refreshTTL := time.Until(registered.RefreshExpiresAt)
	if refreshTTL < 6*24*time.Hour || refreshTTL > defaultRefreshTokenTTL+time.Minute {
		t.Fatalf("unexpected refresh ttl: %s", refreshTTL)
	}
	refreshed, err := store.Refresh(refreshRequest{RefreshToken: registered.RefreshToken})
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Credential == "" || refreshed.Credential == registered.Credential {
		t.Fatalf("expected new access token: %+v", refreshed)
	}
	if refreshed.RefreshToken == "" || refreshed.RefreshToken == registered.RefreshToken {
		t.Fatalf("expected rotated refresh token: %+v", refreshed)
	}
	if _, ok := store.Credential(refreshed.Credential); !ok {
		t.Fatal("expected refreshed credential to authorize")
	}
	if _, err := store.Refresh(refreshRequest{RefreshToken: registered.RefreshToken}); err == nil {
		t.Fatal("old refresh token should be single-use")
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
	slowDown, err := store.PollDeviceAuth(devicePollRequest{DeviceCode: start.DeviceCode})
	if err != nil {
		t.Fatal(err)
	}
	if slowDown.Status != "slow_down" || slowDown.Interval != deviceSlowDownSeconds {
		t.Fatalf("expected slow_down, got %+v", slowDown)
	}
	info, ok := store.DeviceAuthInfo(start.UserCode)
	if !ok {
		t.Fatal("expected device auth info")
	}
	if info.DeviceID != "cli" || info.DeviceName != "CLI" || info.Status != "pending" {
		t.Fatalf("unexpected device info: %+v", info)
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
	if approved.RefreshToken == "" || approved.RefreshExpiresAt.IsZero() {
		t.Fatalf("expected approved refresh token: %+v", approved)
	}
	polled, err := store.PollDeviceAuth(devicePollRequest{DeviceCode: start.DeviceCode})
	if err != nil {
		t.Fatal(err)
	}
	if polled.Status != "approved" || polled.Credential == nil || polled.Credential.Credential != approved.Credential {
		t.Fatalf("unexpected poll response: %+v", polled)
	}
	if polled.Credential.RefreshToken != approved.RefreshToken {
		t.Fatalf("expected poll to include refresh token: %+v", polled.Credential)
	}
	if _, ok := store.DeviceAuthInfo(start.UserCode); ok {
		t.Fatal("device auth info should be consumed after successful poll")
	}
}

func TestAuthStoreDeviceLoginApprovesCurrentUser(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		testAuthStoreDeviceLoginApprovesCurrentUser(t, newAuthStore())
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
		testAuthStoreDeviceLoginApprovesCurrentUser(t, store)
	})
}

func testAuthStoreDeviceLoginApprovesCurrentUser(t *testing.T, store AuthStore) {
	t.Helper()
	registered, err := store.Register(registerRequest{
		Email: "current@example.com", Password: "password123", Name: "Current",
	})
	if err != nil {
		t.Fatal(err)
	}
	start, err := store.StartDeviceAuth(deviceStartRequest{DeviceID: "cli", DeviceName: "CLI"})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := store.ApproveDeviceAuthForUser("current@example.com", start.UserCode)
	if err != nil {
		t.Fatal(err)
	}
	if approved.TenantID != registered.TenantID || approved.User.Email != registered.User.Email || approved.DeviceID != "cli" {
		t.Fatalf("unexpected approved credential: %+v", approved)
	}
	if _, err := store.ApproveDeviceAuthForUser("current@example.com", start.UserCode); err == nil {
		t.Fatal("device code should not approve twice")
	}
	polled, err := store.PollDeviceAuth(devicePollRequest{DeviceCode: start.DeviceCode})
	if err != nil {
		t.Fatal(err)
	}
	if polled.Status != "approved" || polled.Credential == nil || polled.Credential.Credential != approved.Credential {
		t.Fatalf("unexpected poll response: %+v", polled)
	}
}

func TestAuthStoreDeviceLoginBlocksRepeatedFailures(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		testAuthStoreDeviceLoginBlocksRepeatedFailures(t, newAuthStore())
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
		testAuthStoreDeviceLoginBlocksRepeatedFailures(t, store)
	})
}

func testAuthStoreDeviceLoginBlocksRepeatedFailures(t *testing.T, store AuthStore) {
	t.Helper()
	if _, err := store.Register(registerRequest{
		Email: "blocked@example.com", Password: "password123", Name: "Blocked",
	}); err != nil {
		t.Fatal(err)
	}
	start, err := store.StartDeviceAuth(deviceStartRequest{DeviceID: "cli", DeviceName: "CLI"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxDeviceApproveFailures; i++ {
		if _, err := store.ApproveDeviceAuth(deviceApproveRequest{
			UserCode: start.UserCode, Email: "blocked@example.com", Password: "wrong-password",
		}); err == nil {
			t.Fatal("expected failed approval")
		}
	}
	poll, err := store.PollDeviceAuth(devicePollRequest{DeviceCode: start.DeviceCode})
	if err != nil {
		t.Fatal(err)
	}
	if poll.Status != "blocked" {
		t.Fatalf("expected blocked status, got %+v", poll)
	}
	if _, err := store.ApproveDeviceAuth(deviceApproveRequest{
		UserCode: start.UserCode, Email: "blocked@example.com", Password: "password123",
	}); err == nil {
		t.Fatal("blocked device should not approve later")
	}
}

func TestRandomUserCodeUsesLongGroupedSecret(t *testing.T) {
	code, err := randomUserCode()
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != len("AAAA-AAAA-AAAA-AAAA") {
		t.Fatalf("unexpected code length: %q", code)
	}
	if normalizeUserCode(strings.ReplaceAll(code, "-", "")) != code {
		t.Fatalf("normalization should restore grouping: %q", code)
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
