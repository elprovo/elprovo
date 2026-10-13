package setup

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

// GoogleCloud is a Google Cloud project to bootstrap.
type GoogleCloud struct {
	// Credentials is a service account key file. The module, the agent and
	// the registry sign in with it.
	Credentials []byte
	// Project defaults to the project of the service account key.
	Project string
	Region  string
	Zone    string
	// ComputeEndpoint, ArtifactRegistryEndpoint, IAMEndpoint and
	// IAMCredentialsEndpoint are API base URLs; empty means Google's.
	ComputeEndpoint          string
	ArtifactRegistryEndpoint string
	IAMEndpoint              string
	IAMCredentialsEndpoint   string
	RegistryName             string
	// GoldenImage is an image from images/google-cloud that the agent boots
	// preview machines and build workers from. Empty means the stock image
	// with cloud-init.
	GoldenImage string
}

// Cloud returns what setup needs to bootstrap the project with module.
func (g GoogleCloud) Cloud(module fs.FS) (Cloud, error) {
	var key bytes.Buffer
	if err := json.Compact(&key, g.Credentials); err != nil {
		return Cloud{}, fmt.Errorf("read the service account key: %w", err)
	}
	var file struct {
		ProjectID   string `json:"project_id"`
		ClientEmail string `json:"client_email"`
	}
	_ = json.Unmarshal(key.Bytes(), &file)
	if g.Project == "" {
		g.Project = file.ProjectID
	}
	if file.ClientEmail == "" {
		return Cloud{}, errors.New("the service account key names no client_email")
	}
	if g.Project == "" || g.Region == "" || g.Zone == "" || g.RegistryName == "" {
		return Cloud{}, errors.New("google cloud setup needs a project, a region, a zone and a registry name")
	}
	if !strings.HasPrefix(g.Zone, g.Region+"-") {
		return Cloud{}, fmt.Errorf("zone %s is not in region %s", g.Zone, g.Region)
	}
	// The agent's environment file keeps each value on one line.
	encoded := base64.StdEncoding.EncodeToString(key.Bytes())
	return Cloud{
		Provider: "gcp",
		Module:   module,
		TofuEnv:  []string{"GOOGLE_CREDENTIALS=" + key.String()},
		Vars: map[string]any{
			"project": g.Project, "region": g.Region, "zone": g.Zone, "registry_name": g.RegistryName, "agent_service_account": file.ClientEmail,
			"compute_endpoint": g.ComputeEndpoint, "artifact_registry_endpoint": g.ArtifactRegistryEndpoint, "iam_endpoint": g.IAMEndpoint,
		},
		Agent: func(o map[string]any) ([]string, map[string]string, error) {
			registry, err := outputString(o, "registry")
			if err != nil {
				return nil, nil, err
			}
			args := []string{"-gcp-project", g.Project, "-gcp-zone", g.Zone, "-registry", registry}
			for _, flag := range [][2]string{
				{"-machine-prefix", "machine_prefix"},
				{"-gcp-subnetwork", "subnetwork"},
				{"-gcp-tag", "preview_tag"},
				{"-gcp-registry-push-account", "registry_push_account"},
				{"-gcp-registry-pull-account", "registry_pull_account"},
			} {
				value, err := outputString(o, flag[1])
				if err != nil {
					return nil, nil, err
				}
				args = append(args, flag[0], value)
			}
			for _, flag := range [][2]string{
				{"-gcp-compute-url", g.ComputeEndpoint}, {"-gcp-iam-credentials-url", g.IAMCredentialsEndpoint}, {"-gcp-golden-image", g.GoldenImage},
			} {
				if flag[1] != "" {
					args = append(args, flag[0], flag[1])
				}
			}
			return args, map[string]string{"GOOGLE_CREDENTIALS": encoded}, nil
		},
	}, nil
}
