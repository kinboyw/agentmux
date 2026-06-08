package control

import "testing"

func TestControlURL(t *testing.T) {
	got, err := controlURL("http://127.0.0.1:8080", "secret")
	if err != nil {
		t.Fatal(err)
	}
	want := "ws://127.0.0.1:8080/ws/control?token=secret"
	if got != want {
		t.Fatalf("unexpected url:\n got: %s\nwant: %s", got, want)
	}
}

func TestContainsDetachKey(t *testing.T) {
	if !containsDetachKey([]byte{'a', 0x1d}) {
		t.Fatal("expected ctrl-] to detach")
	}
	if containsDetachKey([]byte("hello")) {
		t.Fatal("plain input should not detach")
	}
}
