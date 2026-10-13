package controlplane

import "testing"

func TestSessionURLUsesTheAgentHubOfTheServer(t *testing.T) {
	for server, want := range map[string]string{
		HostedURL:                      "wss://app.elprovo.com/v1/connect",
		"https://elprovo.example.com/": "wss://elprovo.example.com/v1/connect",
		"http://localhost:8080":        "ws://localhost:8080/v1/connect",
	} {
		if got := SessionURL(server); got != want {
			t.Errorf("SessionURL(%q) = %q, want %q", server, got, want)
		}
	}
}
