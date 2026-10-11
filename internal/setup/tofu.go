package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
)

// Tofu applies a bootstrap module with OpenTofu or Terraform. Dir keeps the
// module, its variables and its state between runs; losing the state means
// a later run can't see the resources it made.
type Tofu struct {
	Binary string
	Dir    string
	Env    []string
	Out    io.Writer
}

// Apply writes module into Dir with vars, applies it, and returns its
// outputs.
func (t Tofu) Apply(ctx context.Context, module fs.FS, vars map[string]any) (map[string]any, error) {
	if err := os.MkdirAll(t.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", t.Dir, err)
	}
	if err := copyModule(module, t.Dir); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(vars, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(t.Dir, "terraform.tfvars.json"), data, 0o600); err != nil {
		return nil, fmt.Errorf("write variables: %w", err)
	}
	if err := t.run(ctx, t.Out, "init", "-input=false", "-no-color"); err != nil {
		return nil, err
	}
	if err := t.run(ctx, t.Out, "apply", "-auto-approve", "-input=false", "-no-color"); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := t.run(ctx, &out, "output", "-json"); err != nil {
		return nil, err
	}
	var outputs map[string]struct {
		Value any `json:"value"`
	}
	if err := json.Unmarshal(out.Bytes(), &outputs); err != nil {
		return nil, fmt.Errorf("read outputs: %w", err)
	}
	values := make(map[string]any, len(outputs))
	for k, v := range outputs {
		values[k] = v.Value
	}
	return values, nil
}

func (t Tofu) run(ctx context.Context, stdout io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, t.Binary, args...)
	cmd.Dir = t.Dir
	cmd.Env = append(os.Environ(), t.Env...)
	cmd.Stdout, cmd.Stderr = stdout, t.Out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", filepath.Base(t.Binary), args[0], err)
	}
	return nil
}

func copyModule(module fs.FS, dir string) error {
	return fs.WalkDir(module, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(module, path)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(path)), data, 0o600); err != nil {
			return fmt.Errorf("write module file %s: %w", path, err)
		}
		return nil
	})
}
