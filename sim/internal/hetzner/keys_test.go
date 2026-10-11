package hetzner

import (
	"net/http"
	"net/url"
	"testing"
)

func TestSSHKeyIsFoundByItsMD5Fingerprint(t *testing.T) {
	e := newEnv(t)
	var created struct {
		SSHKey sshKeyJSON `json:"ssh_key"`
	}
	e.mustCall(http.StatusCreated, http.MethodPost, "/v1/ssh_keys", map[string]any{"name": "agent", "public_key": agentKey + "\n"}, &created)

	const fingerprint = "d9:75:02:7a:8d:57:ab:eb:d0:d4:0d:46:4f:fe:24:c4"
	var listed struct {
		SSHKeys []sshKeyJSON `json:"ssh_keys"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v1/ssh_keys?fingerprint="+url.QueryEscape(fingerprint), nil, &listed)

	if created.SSHKey.Fingerprint != fingerprint || created.SSHKey.PublicKey != agentKey+"\n" {
		t.Fatalf("created key = %+v; want its MD5 fingerprint and the public key as sent", created.SSHKey)
	}
	if len(listed.SSHKeys) != 1 || listed.SSHKeys[0].ID != created.SSHKey.ID {
		t.Fatalf("keys with the fingerprint = %+v", listed.SSHKeys)
	}
}

func TestSSHKeyNamesAndFingerprintsAreUnique(t *testing.T) {
	e := newEnv(t)
	e.mustCall(http.StatusCreated, http.MethodPost, "/v1/ssh_keys", map[string]any{"name": "agent", "public_key": agentKey}, nil)

	if status, code := e.errorCode(http.MethodPost, "/v1/ssh_keys", map[string]any{"name": "again", "public_key": agentKey}); status != http.StatusConflict || code != "uniqueness_error" {
		t.Fatalf("the same key again = %d %s, want 409 uniqueness_error", status, code)
	}
	if status, code := e.errorCode(http.MethodPost, "/v1/ssh_keys", map[string]any{"name": "agent", "public_key": "ssh-ed25519 AAAA"}); status != http.StatusUnprocessableEntity || code != "invalid_input" {
		t.Fatalf("a malformed key = %d %s, want 422 invalid_input", status, code)
	}
}
