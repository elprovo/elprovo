package gcp

import (
	"cmp"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/stagely-dev/stagely/sim/internal/registrytoken"
)

const registryCertFileName = "registry-token.pem"

// registryTokens serves the Docker token service of Artifact Registry's
// registry hosts. It signs a registry in with the user name
// oauth2accesstoken and an access token, or with _json_key or
// _json_key_base64 and a service account key file, and grants pull and
// push on the Docker repositories of the project in the service's location.
func (s *Simulation) registryTokens() *registrytoken.Service {
	var signer *registrytoken.Signer
	s.store.Read(func(w *world) { signer = w.Registry })
	return &registrytoken.Service{
		Realm: "Artifact Registry", Signer: signer, Now: func() time.Time { return s.now() }, Log: s.log,
		Authenticate: func(user, password string) (registrytoken.Identity, bool) {
			identity, known := s.registryIdentity(user, password)
			allowed := func(service, repository string) []string { return s.allowedActions(identity, service, repository) }
			return registrytoken.Identity{Subject: cmp.Or(identity, user), Allowed: allowed}, known
		},
	}
}

// registryIdentity returns the service account the credentials belong to,
// "" for a -gcp-access-token, and false when the simulation does not know
// them.
func (s *Simulation) registryIdentity(user, password string) (string, bool) {
	switch user {
	case "oauth2accesstoken":
		return s.tokenOwner(password)
	case "_json_key_base64":
		decoded, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(password), ""))
		if err == nil {
			return s.keyFileIdentity(decoded)
		}
	case "_json_key":
		return s.keyFileIdentity([]byte(password))
	}
	return "", false
}

func (s *Simulation) keyFileIdentity(data []byte) (string, bool) {
	var k keyFile
	if json.Unmarshal(data, &k) != nil {
		return "", false
	}
	known := false
	s.store.Read(func(w *world) {
		known = slices.ContainsFunc(w.Keys, func(key *serviceAccountKey) bool {
			return key.ID == k.PrivateKeyID && key.Email == k.ClientEmail && strings.TrimSpace(key.PrivateKey) == strings.TrimSpace(k.PrivateKey)
		})
	})
	return k.ClientEmail, known
}

// allowedActions returns the actions that the identity may take on a
// repository such as <project>/<repository>/<image> at the registry host
// <location>-docker.pkg.dev: pull and push on an existing Docker repository
// of the project in the location, as far as the identity's role allows. The
// project's owners may do both; another service account may pull with
// roles/artifactregistry.reader on the repository, and push too with a
// writer role.
func (s *Simulation) allowedActions(identity, service, repository string) []string {
	location := strings.TrimSuffix(service, "-docker.pkg.dev")
	path := strings.SplitN(repository, "/", 3)
	if len(path) != 3 || path[0] != s.cfg.project || path[2] == "" {
		return nil
	}
	var allowed []string
	name := "projects/" + s.cfg.project + "/locations/" + location + "/repositories/" + path[1]
	owner := s.isOwner(identity)
	s.store.Read(func(w *world) {
		if repo := w.repository(name); repo != nil {
			allowed = repositoryActions(repo, owner, identity)
		}
	})
	return allowed
}

func repositoryActions(repo *repository, owner bool, identity string) []string {
	if owner {
		return []string{"pull", "push"}
	}
	member := "serviceAccount:" + identity
	switch {
	case repo.Policy.has(member, "roles/artifactregistry.writer", "roles/artifactregistry.repoAdmin", "roles/artifactregistry.admin"):
		return []string{"pull", "push"}
	case repo.Policy.has(member, "roles/artifactregistry.reader"):
		return []string{"pull"}
	}
	return nil
}
