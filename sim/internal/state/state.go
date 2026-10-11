// Package state keeps a simulation's state in memory and saves it as JSON
// after every change, so a restarted simulation keeps its objects.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// Store holds one value of type T. Read and Update serialize access.
type Store[T any] struct {
	mu   sync.Mutex
	path string
	data T
}

// Open loads the value saved at path, or starts from initial when there is
// none yet. An empty path keeps the value in memory only.
func Open[T any](path string, initial T) (*Store[T], error) {
	s := &Store[T]{path: path, data: initial}
	if path == "" {
		return s, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, &s.data); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	return s, nil
}

// Read calls fn with the current value. fn must not keep the pointer.
func (s *Store[T]) Read(fn func(*T)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.data)
}

// Update calls fn with the current value and saves the result. When fn
// returns an error, the value is still kept in memory as fn left it, but
// nothing is saved; fn should change the value only after it validated.
func (s *Store[T]) Update(fn func(*T) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := fn(&s.data); err != nil {
		return err
	}
	return s.save()
}

func (s *Store[T]) save() error {
	if s.path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	return nil
}
