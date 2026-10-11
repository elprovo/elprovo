package github

import (
	"net/http"
	"slices"
	"strings"
	"time"
)

func requireApp(c caller) error {
	if c.kind != callerApp {
		return fail(http.StatusUnauthorized, "%s", errJWTDecode)
	}
	return nil
}

func (s *Sim) getApp(w http.ResponseWriter, r *http.Request, c caller) {
	if err := requireApp(c); err != nil {
		s.writeError(w, r, err)
		return
	}
	var out appJSON
	s.read(func(v view) {
		out = v.app(v.w.app(c.appID))
		n := 0
		for _, i := range v.w.Installations {
			if i.AppID == c.appID {
				n++
			}
		}
		out.InstallationsCount = &n
	})
	writeJSON(w, http.StatusOK, out)
}

func (s *Sim) listAppInstallations(w http.ResponseWriter, r *http.Request, c caller) {
	if err := requireApp(c); err != nil {
		s.writeError(w, r, err)
		return
	}
	var all []installationJSON
	s.read(func(v view) {
		for _, i := range v.w.Installations {
			if i.AppID == c.appID {
				all = append(all, v.installation(i))
			}
		}
	})
	lo, hi := s.paginate(w, r, len(all))
	writeJSON(w, http.StatusOK, orEmpty(all[lo:hi]))
}

func (v view) appInstallation(c caller, r *http.Request) (*installation, error) {
	if err := requireApp(c); err != nil {
		return nil, err
	}
	id, err := pathInt(r, "id")
	if err != nil {
		return nil, err
	}
	inst := v.w.installation(id)
	if inst == nil || inst.AppID != c.appID {
		return nil, errNotFound()
	}
	return inst, nil
}

func (s *Sim) getAppInstallation(w http.ResponseWriter, r *http.Request, c caller) {
	var out installationJSON
	var err error
	s.read(func(v view) {
		var inst *installation
		if inst, err = v.appInstallation(c, r); err == nil {
			out = v.installation(inst)
		}
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Sim) changeInstallation(w http.ResponseWriter, r *http.Request, c caller, status int, change func(t *tx, inst *installation, botID int64)) {
	err := s.update(func(t *tx) error {
		inst, err := t.appInstallation(c, r)
		if err != nil {
			return err
		}
		change(t, inst, t.w.app(c.appID).BotID)
		return nil
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(status)
}

func (s *Sim) deleteAppInstallation(w http.ResponseWriter, r *http.Request, c caller) {
	s.changeInstallation(w, r, c, http.StatusAccepted, (*tx).uninstall)
}

func (s *Sim) suspendInstallation(w http.ResponseWriter, r *http.Request, c caller) {
	s.changeInstallation(w, r, c, http.StatusNoContent, (*tx).suspend)
}

func (s *Sim) unsuspendInstallation(w http.ResponseWriter, r *http.Request, c caller) {
	s.changeInstallation(w, r, c, http.StatusNoContent, (*tx).unsuspend)
}

type tokenRequest struct {
	Repositories  []string          `json:"repositories"`
	RepositoryIDs []int64           `json:"repository_ids"`
	Permissions   map[string]string `json:"permissions"`
}

type installationTokenJSON struct {
	Token               string            `json:"token"`
	ExpiresAt           time.Time         `json:"expires_at"`
	Permissions         map[string]string `json:"permissions"`
	RepositorySelection string            `json:"repository_selection"`
	Repositories        []repoJSON        `json:"repositories,omitempty"`
}

func (s *Sim) createInstallationToken(w http.ResponseWriter, r *http.Request, c caller) {
	var req tokenRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	var out installationTokenJSON
	err := s.update(func(t *tx) error {
		inst, err := t.appInstallation(c, r)
		if err != nil {
			return err
		}
		out, err = t.issueInstallationToken(inst, req)
		return err
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (t *tx) issueInstallationToken(inst *installation, req tokenRequest) (installationTokenJSON, error) {
	if inst.SuspendedAt != nil {
		return installationTokenJSON{}, fail(http.StatusForbidden, "This installation has been suspended")
	}
	repos, err := t.narrowRepos(inst, req)
	if err != nil {
		return installationTokenJSON{}, err
	}
	for name, level := range req.Permissions {
		if permLevel(level) == 0 || permLevel(level) > permLevel(withMetadata(inst.Permissions)[name]) {
			return installationTokenJSON{}, fail(http.StatusUnprocessableEntity, "The permissions requested are not granted to this installation.")
		}
	}
	tok := token{Kind: kindInstallation, AppID: inst.AppID, InstallationID: inst.ID}
	out := installationTokenJSON{Permissions: inst.Permissions, RepositorySelection: inst.RepositorySelection}
	if len(req.Permissions) > 0 {
		tok.Permissions = req.Permissions
		out.Permissions = req.Permissions
	}
	if repos != nil {
		out.RepositorySelection = selectionSelected
		for _, r := range repos {
			tok.RepoIDs = append(tok.RepoIDs, r.ID)
			out.Repositories = append(out.Repositories, t.repo(r))
		}
	}
	issued := t.w.issueToken(tok, installationTokenTTL)
	out.Token, out.ExpiresAt = issued.Value, *issued.ExpiresAt
	return out, nil
}

func (t *tx) narrowRepos(inst *installation, req tokenRequest) ([]*repo, error) {
	if req.Repositories == nil && req.RepositoryIDs == nil {
		return nil, nil
	}
	covered := t.w.installationRepos(inst)
	var out []*repo
	for _, r := range covered {
		if slices.Contains(req.RepositoryIDs, r.ID) || slices.ContainsFunc(req.Repositories, func(n string) bool { return strings.EqualFold(n, r.Name) }) {
			out = append(out, r)
		}
	}
	if len(out) != len(req.Repositories)+len(req.RepositoryIDs) || len(out) == 0 {
		return nil, fail(http.StatusUnprocessableEntity, "There is at least one repository that does not exist or is not accessible to the parent installation.")
	}
	return out, nil
}

type installationReposJSON struct {
	TotalCount          int        `json:"total_count"`
	RepositorySelection string     `json:"repository_selection"`
	Repositories        []repoJSON `json:"repositories"`
}

func (s *Sim) listInstallationRepos(w http.ResponseWriter, r *http.Request, c caller) {
	if c.kind != callerInstallation {
		s.writeError(w, r, fail(http.StatusForbidden, "Resource not accessible by integration"))
		return
	}
	var out installationReposJSON
	var err error
	s.read(func(v view) {
		inst := v.w.installation(c.installationID)
		if inst == nil {
			err = fail(http.StatusUnauthorized, "%s", badCredentials{})
			return
		}
		out.RepositorySelection = inst.RepositorySelection
		if c.repoIDs != nil {
			out.RepositorySelection = selectionSelected
		}
		for _, rp := range v.w.installationRepos(inst) {
			if c.repoIDs == nil || slices.Contains(c.repoIDs, rp.ID) {
				out.Repositories = append(out.Repositories, v.repo(rp))
			}
		}
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out.TotalCount = len(out.Repositories)
	lo, hi := s.paginate(w, r, out.TotalCount)
	out.Repositories = orEmpty(out.Repositories[lo:hi])
	writeJSON(w, http.StatusOK, out)
}

type userInstallationsJSON struct {
	TotalCount    int                `json:"total_count"`
	Installations []installationJSON `json:"installations"`
}

func requireUserToken(c caller) error {
	if c.kind != callerUser {
		return fail(http.StatusForbidden, "You must authenticate with an access token authorized to a GitHub App in order to list installations")
	}
	return nil
}

func (s *Sim) listUserInstallations(w http.ResponseWriter, r *http.Request, c caller) {
	if err := requireUserToken(c); err != nil {
		s.writeError(w, r, err)
		return
	}
	var out userInstallationsJSON
	s.read(func(v view) {
		for _, inst := range v.w.Installations {
			if inst.AppID == c.appID && v.w.userCanAccessInstallation(c.userID, inst) {
				out.Installations = append(out.Installations, v.installation(inst))
			}
		}
	})
	out.TotalCount = len(out.Installations)
	lo, hi := s.paginate(w, r, out.TotalCount)
	out.Installations = orEmpty(out.Installations[lo:hi])
	writeJSON(w, http.StatusOK, out)
}

func (s *Sim) listUserInstallationRepos(w http.ResponseWriter, r *http.Request, c caller) {
	if err := requireUserToken(c); err != nil {
		s.writeError(w, r, err)
		return
	}
	id, err := pathInt(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var out installationReposJSON
	s.read(func(v view) {
		inst := v.w.installation(id)
		if inst == nil || inst.AppID != c.appID || !v.w.userCanAccessInstallation(c.userID, inst) {
			err = errNotFound()
			return
		}
		out.RepositorySelection = inst.RepositorySelection
		for _, rp := range v.w.installationRepos(inst) {
			if v.w.userRole(c.userID, rp) != "" {
				out.Repositories = append(out.Repositories, v.repoForUser(rp, c.userID))
			}
		}
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out.TotalCount = len(out.Repositories)
	lo, hi := s.paginate(w, r, out.TotalCount)
	out.Repositories = orEmpty(out.Repositories[lo:hi])
	writeJSON(w, http.StatusOK, out)
}
