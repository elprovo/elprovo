package gitrepo

import (
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
)

// ServeHTTP serves the git smart HTTP protocol with git http-backend for
// the repositories under root. The request path, relative to root, names
// the repository. remoteUser enables pushes when it is not empty.
func ServeHTTP(w http.ResponseWriter, r *http.Request, root, remoteUser string) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		http.Error(w, "git is not installed", http.StatusInternalServerError)
		return
	}
	cleanup, err := bufferChunkedBody(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer cleanup()

	env := append([]string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}, isolatedEnv...)
	if remoteUser != "" {
		env = append(env, "REMOTE_USER="+remoteUser)
	}
	h := &cgi.Handler{Path: gitPath, Args: []string{"http-backend"}, Env: env}
	h.ServeHTTP(w, r)
}

func bufferChunkedBody(r *http.Request) (func(), error) {
	if r.ContentLength >= 0 {
		return func() {}, nil
	}
	f, err := os.CreateTemp("", "stagely-sim-push-*")
	if err != nil {
		return nil, fmt.Errorf("buffer request body: %w", err)
	}
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}
	n, err := io.Copy(f, r.Body)
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("buffer request body: %w", err)
	}
	r.Body = f
	r.ContentLength = n
	r.TransferEncoding = nil
	return cleanup, nil
}
