package controlplane

import (
	"net/url"
	"strings"
)

// HostedURL is the URL of the control plane that Elprovo runs. It serves the
// console, the JSON API, MCP and the sessions of agents and gateways.
const HostedURL = "https://app.elprovo.com"

// SessionURL returns the WebSocket URL of the agent hub of the control plane
// at server: wss for an https server, ws otherwise.
func SessionURL(server string) string {
	u, err := url.Parse(strings.TrimRight(server, "/"))
	if err != nil {
		return server
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path += "/v1/connect"
	return u.String()
}
