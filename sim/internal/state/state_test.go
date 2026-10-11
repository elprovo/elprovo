package state

import (
	"errors"
	"path/filepath"
	"testing"
)

type counter struct {
	N int `json:"n"`
}

func TestUpdatedValueSurvivesReopening(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(path, counter{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Update(func(c *counter) error { c.N = 3; return nil }); err != nil {
		t.Fatalf("Update: %v", err)
	}

	reopened, err := Open(path, counter{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	var got int
	reopened.Read(func(c *counter) { got = c.N })
	if got != 3 {
		t.Fatalf("N = %d after reopening, want 3", got)
	}
}

func TestFailedUpdateSavesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := Open(path, counter{})
	boom := errors.New("invalid")

	if err := s.Update(func(c *counter) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("Update err = %v, want the callback's error", err)
	}
	reopened, _ := Open(path, counter{N: 7})
	var got int
	reopened.Read(func(c *counter) { got = c.N })
	if got != 7 {
		t.Fatalf("N = %d, want the initial value because nothing was saved", got)
	}
}
