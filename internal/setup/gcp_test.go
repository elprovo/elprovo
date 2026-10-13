package setup

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

const testServiceAccountKey = `{
  "type": "service_account",
  "project_id": "acme-prod",
  "private_key_id": "key-1",
  "private_key": "-----BEGIN PRIVATE KEY-----\nMIIE\n-----END PRIVATE KEY-----\n",
  "client_email": "elprovo@acme-prod.iam.gserviceaccount.com",
  "token_uri": "https://oauth2.googleapis.com/token"
}`

func gcpOutputs() map[string]any {
	return map[string]any{
		"subnetwork": "projects/acme-prod/regions/us-central1/subnetworks/prod-elprovo", "preview_tag": "prod-preview",
		"registry": "us-central1-docker.pkg.dev/acme-prod/elprovo", "machine_prefix": "elprovo-prod-",
		"registry_push_account": "prod-push@acme-prod.iam.gserviceaccount.com", "registry_pull_account": "prod-pull@acme-prod.iam.gserviceaccount.com",
	}
}

func TestGoogleCloudGivesTheAgentItsFlagsAndTheKey(t *testing.T) {
	g := GoogleCloud{
		Credentials: []byte(testServiceAccountKey), Region: "us-central1", Zone: "us-central1-a", RegistryName: "elprovo",
		ComputeEndpoint: "http://sim:9320/compute/v1/", IAMEndpoint: "http://sim:9320/v1/", IAMCredentialsEndpoint: "http://sim:9320/v1/",
	}

	cloud, err := g.Cloud(fstest.MapFS{})
	if err != nil {
		t.Fatalf("Cloud: %v", err)
	}

	compact := `{"type":"service_account","project_id":"acme-prod","private_key_id":"key-1","private_key":"-----BEGIN PRIVATE KEY-----\nMIIE\n-----END PRIVATE KEY-----\n","client_email":"elprovo@acme-prod.iam.gserviceaccount.com","token_uri":"https://oauth2.googleapis.com/token"}`
	if cloud.Provider != "gcp" || cloud.Vars["project"] != "acme-prod" || cloud.Vars["zone"] != "us-central1-a" || cloud.TofuEnv[0] != "GOOGLE_CREDENTIALS="+compact {
		t.Fatalf("cloud = %+v", cloud)
	}
	if cloud.Vars["agent_service_account"] != "elprovo@acme-prod.iam.gserviceaccount.com" || cloud.Vars["iam_endpoint"] != "http://sim:9320/v1/" {
		t.Fatalf("module variables = %v; want the agent's account, which may impersonate the registry accounts", cloud.Vars)
	}
	args, env, err := cloud.Agent(gcpOutputs())
	want := []string{
		"-gcp-project", "acme-prod", "-gcp-zone", "us-central1-a", "-registry", "us-central1-docker.pkg.dev/acme-prod/elprovo",
		"-machine-prefix", "elprovo-prod-", "-gcp-subnetwork", "projects/acme-prod/regions/us-central1/subnetworks/prod-elprovo",
		"-gcp-tag", "prod-preview", "-gcp-registry-push-account", "prod-push@acme-prod.iam.gserviceaccount.com",
		"-gcp-registry-pull-account", "prod-pull@acme-prod.iam.gserviceaccount.com",
		"-gcp-compute-url", "http://sim:9320/compute/v1/", "-gcp-iam-credentials-url", "http://sim:9320/v1/",
	}
	if err != nil || !reflect.DeepEqual(args, want) {
		t.Fatalf("agent args = %v, %v", args, err)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(compact))
	if len(env) != 1 || env["GOOGLE_CREDENTIALS"] != encoded {
		t.Fatalf("agent env = %v; want only the key, and no registry credentials that would reach machines", env)
	}
	svc := service{name: "elprovo-agent", args: args, env: env}
	if _, err := svc.unit(); err != nil {
		t.Fatalf("unit: %v", err)
	}
	if file := svc.envFile(); strings.Count(file, "\n") != 1 || strings.Contains(file, `\`) {
		t.Fatalf("environment file = %q; want the key on one line, with nothing to unescape", file)
	}
	if _, _, err := cloud.Agent(map[string]any{}); err == nil {
		t.Fatal("missing outputs were accepted")
	}
}

func TestGoogleCloudPassesTheGoldenImage(t *testing.T) {
	g := GoogleCloud{Credentials: []byte(testServiceAccountKey), Project: "acme-other", Region: "europe-west1", Zone: "europe-west1-b", RegistryName: "elprovo", GoldenImage: "elprovo-20261001"}

	cloud, err := g.Cloud(fstest.MapFS{})
	if err != nil {
		t.Fatalf("Cloud: %v", err)
	}
	args, _, err := cloud.Agent(gcpOutputs())

	if err != nil || args[1] != "acme-other" || !reflect.DeepEqual(args[len(args)-2:], []string{"-gcp-golden-image", "elprovo-20261001"}) {
		t.Fatalf("agent args = %v, %v; want the given project and -gcp-golden-image", args, err)
	}
	if strings.Contains(strings.Join(args, " "), "key-1") {
		t.Fatal("the key is in the agent's arguments, which other users can read")
	}
}

func TestGoogleCloudRejectsAZoneOutsideTheRegion(t *testing.T) {
	g := GoogleCloud{Credentials: []byte(testServiceAccountKey), Region: "us-central1", Zone: "europe-west1-b", RegistryName: "elprovo"}

	if _, err := g.Cloud(fstest.MapFS{}); err == nil || !strings.Contains(err.Error(), "not in region") {
		t.Fatalf("Cloud err = %v, want the zone rejected", err)
	}
}
