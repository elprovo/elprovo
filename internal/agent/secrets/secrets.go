// Package secrets keeps application secret values on the agent host. The
// store is a file sealed with AES-256-GCM under a key in a separate file;
// a pipeline can also supply values in a dotenv file. The control plane only
// ever names secrets by key.
package secrets

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// ErrInvalidKey means a secret key has characters other than letters,
// digits, dots, dashes and underscores, or is longer than 128 characters.
var ErrInvalidKey = errors.New("secret key must be 1 to 128 letters, digits, dots, dashes or underscores")

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// Store is the encrypted secret file and its key file. Writes replace the
// file atomically, so a running agent always reads a whole store.
type Store struct {
	path    string
	keyPath string
}

// NewStore returns the store at path, sealed with the key at keyPath. Neither
// file has to exist yet.
func NewStore(path, keyPath string) *Store {
	return &Store{path: path, keyPath: keyPath}
}

type sealed struct {
	Version    int    `json:"version"`
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}

// Set stores value under key.
func (s *Store) Set(key, value string) error {
	if !keyPattern.MatchString(key) {
		return fmt.Errorf("%w: %q", ErrInvalidKey, key)
	}
	values, err := s.Values()
	if err != nil {
		return err
	}
	values[key] = value
	return s.write(values)
}

// Delete removes key. Deleting a missing key succeeds.
func (s *Store) Delete(key string) error {
	values, err := s.Values()
	if err != nil {
		return err
	}
	delete(values, key)
	return s.write(values)
}

// Keys returns the stored keys in order.
func (s *Store) Keys() ([]string, error) {
	values, err := s.Values()
	if err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(values)), nil
}

// Values returns every stored secret. A store that does not exist yet is
// empty.
func (s *Store) Values() (map[string]string, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read secret store: %w", err)
	}
	var box sealed
	if err := json.Unmarshal(data, &box); err != nil || box.Version != 1 {
		return nil, fmt.Errorf("secret store %s is not a version 1 store", s.path)
	}
	aead, err := s.cipher(false)
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, box.Nonce, box.Ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("open secret store: the key in %s does not match it", s.keyPath)
	}
	values := map[string]string{}
	if err := json.Unmarshal(plain, &values); err != nil {
		return nil, fmt.Errorf("decode secret store: %w", err)
	}
	return values, nil
}

func (s *Store) write(values map[string]string) error {
	aead, err := s.cipher(true)
	if err != nil {
		return err
	}
	plain, err := json.Marshal(values)
	if err != nil {
		return fmt.Errorf("encode secret store: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	rand.Read(nonce)
	data, err := json.Marshal(sealed{Version: 1, Nonce: nonce, Ciphertext: aead.Seal(nil, nonce, plain, nil)})
	if err != nil {
		return fmt.Errorf("encode secret store: %w", err)
	}
	return writeFileAtomic(s.path, data)
}

// cipher loads the store key, creating it first when create is set.
func (s *Store) cipher(create bool) (cipher.AEAD, error) {
	key, err := os.ReadFile(s.keyPath)
	if errors.Is(err, fs.ErrNotExist) && create {
		key = make([]byte, 32)
		rand.Read(key)
		err = writeFileAtomic(s.keyPath, key)
	}
	if err != nil {
		return nil, fmt.Errorf("read secret store key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("secret store key %s must be 32 bytes", s.keyPath)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secret store key: %w", err)
	}
	return cipher.NewGCM(block)
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		_ = os.Remove(f.Name())
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		_ = os.Remove(f.Name())
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// ReadFile reads secrets from a dotenv file: KEY=value lines, with optional
// single or double quotes around the value. Blank lines and lines that
// start with # are skipped.
func ReadFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read secrets file: %w", err)
	}
	values := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for n := 1; scanner.Scan(); n++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		key = strings.TrimSpace(key)
		if !ok || !keyPattern.MatchString(key) {
			return nil, fmt.Errorf("secrets file %s line %d: want KEY=value with a valid key", path, n)
		}
		values[key] = unquote(strings.TrimSpace(value))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read secrets file: %w", err)
	}
	return values, nil
}

func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

// Resolver finds secret values in the store and in an optional dotenv file.
// The file wins when both hold a key, because a pipeline writes it for the
// current run.
type Resolver struct {
	Store *Store
	File  string
}

// Resolve returns the value of each key. A missing key is an error that
// names the key and never a value.
func (r Resolver) Resolve(keys []string) (map[string]string, error) {
	if len(keys) == 0 {
		return map[string]string{}, nil
	}
	values, err := r.Store.Values()
	if err != nil {
		return nil, err
	}
	if r.File != "" {
		fromFile, err := ReadFile(r.File)
		if err != nil {
			return nil, err
		}
		maps.Copy(values, fromFile)
	}
	out := make(map[string]string, len(keys))
	var missing []string
	for _, k := range keys {
		v, ok := values[k]
		if !ok {
			missing = append(missing, k)
			continue
		}
		out[k] = v
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("the agent has no secret %s; set it with `elprovo-agent secrets set`", strings.Join(quoteAll(missing), ", "))
	}
	return out, nil
}

func quoteAll(keys []string) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = fmt.Sprintf("%q", k)
	}
	return out
}
