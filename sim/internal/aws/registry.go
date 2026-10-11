package aws

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/stagely-dev/stagely/sim/internal/registrytoken"
)

const (
	// registryTokenPath is where the simulation serves the Docker token
	// service that the registry you run sends Docker to.
	registryTokenPath    = "/v2/token"
	registryCertFileName = "registry-token.pem"
)

var (
	// pullActions are the ECR actions that a Docker pull needs: the image
	// manifest and its layers.
	pullActions = []string{"ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer"}
	// pushActions are the ECR actions that a Docker push needs.
	pushActions = []string{"ecr:BatchCheckLayerAvailability", "ecr:InitiateLayerUpload", "ecr:UploadLayerPart", "ecr:CompleteLayerUpload", "ecr:PutImage"}
)

// registryToken is what the password of an ECR authorization token stands
// for: the region whose registry it signs in to and what its caller may do
// there.
type registryToken struct {
	Region  string `json:"region"`
	Subject string `json:"subject"`
	// Policy is the session policy of a federated session's token; nil for
	// the IAM user, who may do everything.
	Policy  *policy   `json:"policy,omitempty"`
	Expires time.Time `json:"expires"`
}

// allowedActions returns pull when the token's caller may get the images
// and layers of the repository at service, and push when it may also put
// them. Only the account's registry in the token's region accepts it.
func (t *registryToken) allowedActions(account, service, repository string) []string {
	if service != registryHost(account, t.Region) {
		return nil
	}
	if t.Policy == nil {
		return []string{"pull", "push"}
	}
	resource := "arn:aws:ecr:" + t.Region + ":" + account + ":repository/" + repository
	var actions []string
	if t.Policy.allowsAll(pullActions, resource) {
		actions = append(actions, "pull")
	}
	if t.Policy.allowsAll(pushActions, resource) {
		actions = append(actions, "push")
	}
	return actions
}

// registryTokens serves the Docker token service of the account's
// registries. Docker signs in with the user AWS and the password of an ECR
// authorization token that has not expired.
func (s *Simulation) registryTokens() *registrytoken.Service {
	var signer *registrytoken.Signer
	var account string
	s.store.Read(func(w *world) { signer, account = w.RegistrySigner, w.AccountID })
	return &registrytoken.Service{
		Realm: "Amazon ECR", Signer: signer, Now: func() time.Time { return s.now() }, Log: s.log,
		Authenticate: func(user, password string) (registrytoken.Identity, bool) {
			var t *registryToken
			s.store.Read(func(w *world) {
				if found := w.RegistryTokens[password]; found != nil {
					copied := *found
					t = &copied
				}
			})
			if user != "AWS" || t == nil || !s.now().Before(t.Expires) {
				return registrytoken.Identity{}, false
			}
			allowed := func(service, repository string) []string { return t.allowedActions(account, service, repository) }
			return registrytoken.Identity{Subject: t.Subject, Allowed: allowed}, true
		},
	}
}

// writeRegistryCert writes the certificate that the registry must trust
// for the token service's tokens next to the state.
func (s *Simulation) writeRegistryCert() error {
	if s.statePath == "" {
		return nil
	}
	var cert string
	s.store.Read(func(w *world) { cert = w.RegistrySigner.Certificate })
	dir := filepath.Dir(s.statePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, registryCertFileName), []byte(cert), 0o644); err != nil {
		return fmt.Errorf("write the registry token certificate: %w", err)
	}
	return nil
}
