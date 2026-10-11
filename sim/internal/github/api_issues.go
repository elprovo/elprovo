package github

import (
	"encoding/json"
	"net/http"
)

func (v view) actor(c caller) (userID, appID int64) {
	switch c.kind {
	case callerInstallation:
		return v.w.app(c.appID).BotID, c.appID
	case callerUser:
		return c.userID, c.appID
	}
	return c.userID, 0
}

func (v view) issueFor(c caller, r *http.Request, level string) (*repo, *pull, error) {
	rp, err := v.repoFor(c, r, level, "issues", "pull_requests")
	if err != nil {
		return nil, nil, err
	}
	number, err := pathInt(r, "number")
	if err != nil {
		return nil, nil, err
	}
	p := rp.pull(int(number))
	if p == nil {
		return nil, nil, errNotFound()
	}
	return rp, p, nil
}

func (s *Sim) listComments(w http.ResponseWriter, r *http.Request, c caller) {
	var all []commentJSON
	var err error
	s.read(func(v view) {
		var rp *repo
		var p *pull
		if rp, p, err = v.issueFor(c, r, permRead); err != nil {
			return
		}
		for _, cm := range rp.Comments {
			if cm.IssueNumber == p.Number {
				all = append(all, v.comment(rp, cm))
			}
		}
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	lo, hi := s.paginate(w, r, len(all))
	writeJSON(w, http.StatusOK, orEmpty(all[lo:hi]))
}

type commentBody struct {
	Body string `json:"body"`
}

func (s *Sim) createComment(w http.ResponseWriter, r *http.Request, c caller) {
	var in commentBody
	if err := decodeJSON(r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	var out commentJSON
	err := s.update(func(t *tx) error {
		rp, p, err := t.issueFor(c, r, permWrite)
		if err != nil {
			return err
		}
		userID, appID := t.actor(c)
		cm, err := t.addComment(rp, p, userID, appID, in.Body)
		if err != nil {
			return err
		}
		out = t.comment(rp, cm)
		return nil
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (t *tx) ownComment(c caller, r *http.Request) (*repo, *comment, int64, error) {
	rp, err := t.repoFor(c, r, permRead, "issues", "pull_requests")
	if err != nil {
		return nil, nil, 0, err
	}
	id, err := pathInt(r, "id")
	if err != nil {
		return nil, nil, 0, err
	}
	cm := rp.comment(id)
	if cm == nil {
		return nil, nil, 0, errNotFound()
	}
	userID, _ := t.actor(c)
	perms := t.w.repoPerms(c, rp)
	if cm.UserID != userID && !allows(perms, "issues", permWrite) && !allows(perms, "pull_requests", permWrite) {
		return nil, nil, 0, errDenied(c)
	}
	return rp, cm, userID, nil
}

func (s *Sim) updateComment(w http.ResponseWriter, r *http.Request, c caller) {
	var in commentBody
	if err := decodeJSON(r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	var out commentJSON
	err := s.update(func(t *tx) error {
		rp, cm, userID, err := t.ownComment(c, r)
		if err != nil {
			return err
		}
		if err := t.editComment(rp, cm, userID, in.Body); err != nil {
			return err
		}
		out = t.comment(rp, cm)
		return nil
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Sim) deleteIssueChild(w http.ResponseWriter, r *http.Request, c caller) {
	switch {
	case r.PathValue("number") == "comments":
		r.SetPathValue("id", r.PathValue("child"))
		s.deleteComment(w, r, c)
	case r.PathValue("child") == "labels":
		s.removeAllIssueLabels(w, r, c)
	default:
		s.writeError(w, r, errNotFound())
	}
}

func (s *Sim) deleteComment(w http.ResponseWriter, r *http.Request, c caller) {
	err := s.update(func(t *tx) error {
		rp, cm, userID, err := t.ownComment(c, r)
		if err != nil {
			return err
		}
		t.deleteComment(rp, cm, userID)
		return nil
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Sim) listIssueLabels(w http.ResponseWriter, r *http.Request, c caller) {
	var all []labelJSON
	var err error
	s.read(func(v view) {
		var rp *repo
		var p *pull
		if rp, p, err = v.issueFor(c, r, permRead); err == nil {
			all = v.labels(rp, p.Labels)
		}
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	lo, hi := s.paginate(w, r, len(all))
	writeJSON(w, http.StatusOK, orEmpty(all[lo:hi]))
}

func parseLabelNames(raw json.RawMessage) ([]string, error) {
	var names []string
	if json.Unmarshal(raw, &names) == nil {
		return names, nil
	}
	var wrapped struct {
		Labels json.RawMessage `json:"labels"`
	}
	if json.Unmarshal(raw, &wrapped) != nil || wrapped.Labels == nil {
		return nil, errValidation("Label", "labels", "missing_field")
	}
	if json.Unmarshal(wrapped.Labels, &names) == nil {
		return names, nil
	}
	var objects []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(wrapped.Labels, &objects); err != nil {
		return nil, errValidation("Label", "labels", "invalid")
	}
	for _, o := range objects {
		names = append(names, o.Name)
	}
	return names, nil
}

func (s *Sim) changeIssueLabels(w http.ResponseWriter, r *http.Request, c caller, change func(t *tx, rp *repo, p *pull, userID int64) error) {
	var out []labelJSON
	err := s.update(func(t *tx) error {
		rp, p, err := t.issueFor(c, r, permWrite)
		if err != nil {
			return err
		}
		userID, _ := t.actor(c)
		if err := change(t, rp, p, userID); err != nil {
			return err
		}
		out = t.labels(rp, p.Labels)
		return nil
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Sim) addIssueLabels(w http.ResponseWriter, r *http.Request, c caller) {
	var raw json.RawMessage
	if err := decodeJSON(r, &raw); err != nil {
		s.writeError(w, r, err)
		return
	}
	names, err := parseLabelNames(raw)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.changeIssueLabels(w, r, c, func(t *tx, rp *repo, p *pull, userID int64) error {
		return t.addLabels(rp, p, userID, names)
	})
}

func (s *Sim) removeIssueLabel(w http.ResponseWriter, r *http.Request, c caller) {
	s.changeIssueLabels(w, r, c, func(t *tx, rp *repo, p *pull, userID int64) error {
		return t.removeLabel(rp, p, userID, r.PathValue("name"))
	})
}

func (s *Sim) removeAllIssueLabels(w http.ResponseWriter, r *http.Request, c caller) {
	err := s.update(func(t *tx) error {
		rp, p, err := t.issueFor(c, r, permWrite)
		if err != nil {
			return err
		}
		userID, _ := t.actor(c)
		for _, name := range append([]string(nil), p.Labels...) {
			if err := t.removeLabel(rp, p, userID, name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
