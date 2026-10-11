package github

import "net/http"

func (s *Sim) getAuthenticatedUser(w http.ResponseWriter, r *http.Request, c caller) {
	switch c.kind {
	case callerUser, callerPersonal:
	case callerAnonymous:
		s.writeError(w, r, fail(http.StatusUnauthorized, "Requires authentication"))
		return
	default:
		s.writeError(w, r, fail(http.StatusForbidden, "Resource not accessible by integration"))
		return
	}
	var out fullUserJSON
	s.read(func(v view) { out = v.fullUser(v.w.account(c.userID), true) })
	writeJSON(w, http.StatusOK, out)
}

func (s *Sim) getUser(w http.ResponseWriter, r *http.Request, _ caller) {
	var out fullUserJSON
	found := false
	s.read(func(v view) {
		if a := v.w.accountByLogin(r.PathValue("login")); a != nil {
			out, found = v.fullUser(a, false), true
		}
	})
	if !found {
		s.writeError(w, r, errNotFound())
		return
	}
	writeJSON(w, http.StatusOK, out)
}
