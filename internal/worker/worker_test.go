package worker

import "testing"

func TestWorkerURL(t *testing.T) {
	got, err := workerURL("https://agents.example.com", "secret")
	if err != nil {
		t.Fatal(err)
	}
	want := "wss://agents.example.com/ws/worker?token=secret"
	if got != want {
		t.Fatalf("unexpected url:\n got: %s\nwant: %s", got, want)
	}
}
