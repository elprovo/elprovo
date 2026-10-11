package github

import "slices"

const (
	permRead  = "read"
	permWrite = "write"
	permAdmin = "admin"
)

var repoPermissionNames = []string{"metadata", "contents", "pull_requests", "issues", "statuses", "deployments"}

func permLevel(p string) int {
	switch p {
	case permRead:
		return 1
	case permWrite:
		return 2
	case permAdmin:
		return 3
	}
	return 0
}

func maxPerm(a, b string) string {
	if permLevel(b) > permLevel(a) {
		return b
	}
	return a
}

func (w *world) userRole(userID int64, r *repo) string {
	role := ""
	if !r.Private {
		role = permRead
	}
	if r.OwnerID == userID {
		return permAdmin
	}
	if owner := w.account(r.OwnerID); owner != nil {
		switch owner.role(userID) {
		case roleAdmin:
			return permAdmin
		case roleMember:
			role = maxPerm(role, permRead)
		}
	}
	for _, c := range r.Collaborators {
		if c.UserID == userID {
			role = maxPerm(role, c.Permission)
		}
	}
	return role
}

func roleGrant(role string) map[string]string {
	if role == "" {
		return nil
	}
	level := permRead
	if permLevel(role) >= permLevel(permWrite) {
		level = permWrite
	}
	perms := map[string]string{}
	for _, name := range repoPermissionNames {
		perms[name] = level
	}
	perms["metadata"] = permRead
	return perms
}

func capToRole(perms map[string]string, role string) map[string]string {
	capped := map[string]string{}
	for name, level := range perms {
		if permLevel(level) > permLevel(role) {
			level = role
		}
		capped[name] = level
	}
	return capped
}

type callerKind int

const (
	callerAnonymous callerKind = iota
	callerApp
	callerInstallation
	callerUser
	callerPersonal
)

type caller struct {
	kind           callerKind
	appID          int64
	installationID int64
	userID         int64
	repoIDs        []int64
	permissions    map[string]string
	rateKey        string
}

func (c caller) isIntegration() bool {
	return c.kind == callerApp || c.kind == callerInstallation || c.kind == callerUser
}

func (w *world) repoPerms(c caller, r *repo) map[string]string {
	public := map[string]string(nil)
	if !r.Private {
		public = roleGrant(permRead)
	}
	switch c.kind {
	case callerInstallation:
		if p := w.installationRepoPerms(c, r); p != nil {
			return p
		}
	case callerUser:
		if p := w.userTokenRepoPerms(c, r); p != nil {
			return p
		}
	case callerPersonal:
		if p := roleGrant(w.userRole(c.userID, r)); p != nil {
			return p
		}
	}
	return public
}

func (w *world) installationRepoPerms(c caller, r *repo) map[string]string {
	inst := w.installation(c.installationID)
	if inst == nil || !inst.covers(r) {
		return nil
	}
	if c.repoIDs != nil && !slices.Contains(c.repoIDs, r.ID) {
		return nil
	}
	perms := inst.Permissions
	if c.permissions != nil {
		perms = c.permissions
	}
	return withMetadata(perms)
}

func (w *world) userTokenRepoPerms(c caller, r *repo) map[string]string {
	role := w.userRole(c.userID, r)
	if role == "" {
		return nil
	}
	for _, inst := range w.Installations {
		if inst.AppID == c.appID && inst.covers(r) {
			return capToRole(withMetadata(inst.Permissions), role)
		}
	}
	return nil
}

func withMetadata(perms map[string]string) map[string]string {
	out := map[string]string{"metadata": permRead}
	for k, v := range perms {
		out[k] = maxPerm(out[k], v)
	}
	return out
}

func allows(perms map[string]string, name, level string) bool {
	return permLevel(perms[name]) >= permLevel(level)
}

func (w *world) userCanAccessInstallation(userID int64, inst *installation) bool {
	if inst.AccountID == userID {
		return true
	}
	owner := w.account(inst.AccountID)
	return owner != nil && owner.role(userID) != ""
}

func (w *world) adminAccounts(userID int64) []*account {
	var out []*account
	for _, a := range w.Accounts {
		if a.ID == userID || (a.Type == typeOrg && a.role(userID) == roleAdmin) {
			out = append(out, a)
		}
	}
	return out
}
