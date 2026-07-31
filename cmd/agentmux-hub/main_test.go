package main

import (
	"encoding/json"
	"testing"

	"private/agentmux/internal/protocol"
)

func TestParseP2PICEServersCommaSeparated(t *testing.T) {
	servers, err := parseP2PICEServers("stun:stun1.example.net:3478, turn:turn.example.net:3478?transport=udp")
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 {
		t.Fatalf("got %d servers, want 1", len(servers))
	}
	if got, want := len(servers[0].URLs), 2; got != want {
		t.Fatalf("got %d urls, want %d", got, want)
	}
}

func TestParseP2PICEServersJSON(t *testing.T) {
	raw, err := json.Marshal([]protocol.P2PICEServer{
		{
			URLs:       []string{"stun:turn.example.net:3478"},
			Username:   " agentmux ",
			Credential: " secret ",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	servers, err := parseP2PICEServers(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 {
		t.Fatalf("got %d servers, want 1", len(servers))
	}
	if got, want := servers[0].Username, "agentmux"; got != want {
		t.Fatalf("got username %q, want %q", got, want)
	}
	if got, want := servers[0].Credential, "secret"; got != want {
		t.Fatalf("got credential %q, want %q", got, want)
	}
}
