package sshrun

import (
	"context"
	"errors"
	"testing"

	"golang.org/x/crypto/ssh"
)

func newMachines(t *testing.T) Machines {
	t.Helper()
	return Machines{
		Port: "22", HostKeyDir: t.TempDir(),
		Address: func(context.Context, string) (string, error) { return "192.0.2.10", nil },
	}
}

func TestNewHostKeyPinsThePublicHalfOfTheKeyItReturns(t *testing.T) {
	m := newMachines(t)

	pemKey, err := m.NewHostKey("elprovo-5e1a9c0b7d42")
	if err != nil {
		t.Fatalf("NewHostKey: %v", err)
	}

	signer, err := ssh.ParsePrivateKey([]byte(pemKey))
	if err != nil {
		t.Fatalf("the returned key does not parse: %v", err)
	}
	pinned, err := m.pinnedHostKey("elprovo-5e1a9c0b7d42")
	if err != nil {
		t.Fatalf("pinned host key: %v", err)
	}
	if string(pinned.Marshal()) != string(signer.PublicKey().Marshal()) {
		t.Fatal("the pinned key is not the public half of the returned key")
	}
}

func TestRunRefusesAMachineWithoutAPinnedHostKey(t *testing.T) {
	m := newMachines(t)
	if _, err := m.NewHostKey("elprovo-build-1"); err != nil {
		t.Fatal(err)
	}

	if err := m.Forget("elprovo-build-1"); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	err := m.Run(t.Context(), "elprovo-build-1", nil, nil, nil, "true")
	if !errors.Is(err, ErrNoHostKey) {
		t.Fatalf("Run after Forget = %v, want ErrNoHostKey", err)
	}
	if err := m.Forget("elprovo-build-1"); err != nil {
		t.Fatalf("forgetting a forgotten machine: %v", err)
	}
}

func TestMachineNamesThatAreNotSafeFileNamesAreRefused(t *testing.T) {
	m := newMachines(t)

	for _, name := range []string{"../agent", "Elprovo", "", "elprovo/build"} {
		if _, err := m.NewHostKey(name); err == nil {
			t.Errorf("NewHostKey(%q) succeeded, want the name refused", name)
		}
	}
}
