package setup

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
)

// loadOrCreateKey reads an Ed25519 key in OpenSSH format from path, or
// creates one that only the user can read. The operator can use the same
// file with `ssh -i`.
func loadOrCreateKey(path string) (ed25519.PrivateKey, error) {
	data, err := loadOrCreateKeyPEM(path)
	if err != nil {
		return nil, err
	}
	raw, err := ssh.ParseRawPrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("parse key %s: %w", path, err)
	}
	key, ok := raw.(*ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("key %s is not an Ed25519 key", path)
	}
	return *key, nil
}

// loadOrCreateKeyPEM returns the OpenSSH private key in path as written,
// creating the file when it is missing.
func loadOrCreateKeyPEM(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return createKey(path)
	}
	if err != nil {
		return nil, fmt.Errorf("read key %s: %w", path, err)
	}
	return data, nil
}

func createKey(path string) ([]byte, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(key, "elprovo setup")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	data := pem.EncodeToMemory(block)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, fmt.Errorf("write key %s: %w", path, err)
	}
	return data, nil
}
