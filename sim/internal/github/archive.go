package github

import (
	"cmp"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

const archiveLinkTTL = 5 * time.Minute

func (s *Sim) getTarball(w http.ResponseWriter, r *http.Request, c caller) {
	h, err := s.contentsHandle(c, r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	sha, err := h.git.ResolveCommit(r.Context(), cmp.Or(r.PathValue("ref"), h.defaultBranch))
	if isNotFound(err) {
		err = errNotFound()
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	expires := time.Now().Add(archiveLinkTTL).Unix()
	location := fmt.Sprintf("%s/codeload/%s/%s/legacy.tar.gz/%s?expires=%d&token=%s",
		s.cfg.WebURL, h.owner, h.name, sha, expires, s.archiveToken(h.owner, h.name, sha, expires))
	w.Header().Set("Location", location)
	w.WriteHeader(http.StatusFound)
}

func (s *Sim) archiveToken(owner, name, sha string, expires int64) string {
	return sign(sha256.New, string(s.archiveKey), []byte(fmt.Sprintf("%s/%s/%s/%d", owner, name, sha, expires)))
}

func (s *Sim) serveArchive(w http.ResponseWriter, r *http.Request) {
	owner, name, sha := r.PathValue("owner"), r.PathValue("repo"), r.PathValue("sha")
	expires, err := strconv.ParseInt(r.URL.Query().Get("expires"), 10, 64)
	want := s.archiveToken(owner, name, sha, expires)
	if err != nil || time.Now().Unix() > expires || !hmac.Equal([]byte(want), []byte(r.URL.Query().Get("token"))) {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	prefix := fmt.Sprintf("%s-%s-%s", owner, name, sha[:min(7, len(sha))])
	w.Header().Set("Content-Type", "application/x-gzip")
	w.Header().Set("Content-Disposition", "attachment; filename="+prefix+".tar.gz")
	if err := s.gitRepo(owner, name).Archive(r.Context(), w, sha, prefix); err != nil {
		s.log.Error("write archive", "repository", owner+"/"+name, "error", err)
	}
}
