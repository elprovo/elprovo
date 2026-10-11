// Package build turns a commit into images on a build worker. The agent
// downloads the source and copies it to the worker, so the worker never
// receives a GitHub token. The worker builds the Compose services and pushes
// their images to the registry by digest.
package build

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// Machine runs commands on a build worker and copies files into it. Run
// returns the standard output; Stream sends the output to the job log.
type Machine interface {
	Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error)
	Stream(ctx context.Context, stdin io.Reader, args ...string) error
	CopyIn(ctx context.Context, localPath, remotePath string) error
}

// Source downloads commit tarballs from the GitHub API.
type Source struct {
	HTTP     *http.Client
	MaxBytes int64
}

// Commit names one commit of a repository, the GitHub API it lives on, and
// the token that reads it.
type Commit struct {
	APIURL     string
	Repository string
	SHA        string
	Token      string
}

var (
	repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	commitPattern     = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

const remoteTarball = "/tmp/stagely-source.tar.gz"

// Upload replaces dir on m with the source of the commit. It refuses a
// source with a symbolic link that resolves outside dir.
func (s *Source) Upload(ctx context.Context, m Machine, c Commit, dir string) error {
	if !repositoryPattern.MatchString(c.Repository) {
		return fmt.Errorf("invalid repository %q", c.Repository)
	}
	if !commitPattern.MatchString(c.SHA) {
		return fmt.Errorf("invalid commit %q", c.SHA)
	}
	if u, err := url.Parse(c.APIURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("invalid GitHub API URL %q", c.APIURL)
	}
	path, err := s.download(ctx, c)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(path) }()

	if err := m.CopyIn(ctx, path, remoteTarball); err != nil {
		return fmt.Errorf("copy source to machine: %w", err)
	}
	steps := [][]string{
		{"sudo", "rm", "-rf", dir},
		{"sudo", "mkdir", "-p", dir},
		{"sudo", "tar", "-xzf", remoteTarball, "-C", dir, "--strip-components=1"},
		{"rm", "-f", remoteTarball},
	}
	for _, step := range steps {
		if _, err := m.Run(ctx, nil, step...); err != nil {
			return fmt.Errorf("unpack source: %w", err)
		}
	}
	return checkLinks(ctx, m, dir)
}

func checkLinks(ctx context.Context, m Machine, dir string) error {
	out, err := m.Run(ctx, nil, "sudo", "find", dir, "-type", "l", "-exec", "realpath", "-m", "-z", "--", "{}", "+")
	if err != nil {
		return fmt.Errorf("check symbolic links: %w", err)
	}
	for target := range strings.SplitSeq(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if target != "" && !within(dir, target) {
			return fmt.Errorf("the source has a symbolic link to %s, which is outside the repository", target)
		}
	}
	return nil
}

func (s *Source) download(ctx context.Context, c Commit) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.APIURL, "/")+"/repos/"+c.Repository+"/tarball/"+c.SHA, nil)
	if err != nil {
		return "", fmt.Errorf("build source request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("download source: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download source: GitHub answered %s", resp.Status)
	}

	f, err := os.CreateTemp("", "stagely-source-*.tar.gz")
	if err != nil {
		return "", fmt.Errorf("create source file: %w", err)
	}
	n, copyErr := io.Copy(f, io.LimitReader(resp.Body, s.MaxBytes+1))
	closeErr := f.Close()
	if err := firstError(copyErr, closeErr); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("write source file: %w", err)
	}
	if n > s.MaxBytes {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("download source: tarball is larger than %d bytes", s.MaxBytes)
	}
	return f.Name(), nil
}

func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
