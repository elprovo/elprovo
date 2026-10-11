package setup

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func TestAWSCloudGivesTheAgentItsFlagsAndCredentials(t *testing.T) {
	a := AWS{AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "secret", Region: "eu-central-1", Endpoint: "http://sim:9310"}
	outputs := map[string]any{
		"subnet_id": "subnet-1", "stagelet_security_group_id": "sg-1", "stagelet_group": "prod-stagelet",
		"image_id": "ami-1", "registry": "123456789012.dkr.ecr.eu-central-1.amazonaws.com/prod",
	}

	cloud, err := a.Cloud(fstest.MapFS{})
	if err != nil {
		t.Fatalf("Cloud: %v", err)
	}
	if cloud.Provider != "aws" || cloud.Vars["endpoint"] != "http://sim:9310" || cloud.Vars["region"] != "eu-central-1" ||
		!slices.Equal(cloud.TofuEnv, []string{"AWS_ACCESS_KEY_ID=AKIAEXAMPLE", "AWS_SECRET_ACCESS_KEY=secret"}) {
		t.Fatalf("cloud = %+v", cloud)
	}
	args, env, err := cloud.Agent(outputs)
	want := []string{
		"-aws-region", "eu-central-1", "-aws-ec2-url", "http://sim:9310", "-aws-ecr-url", "http://sim:9310", "-aws-sts-url", "http://sim:9310",
		"-aws-subnet", "subnet-1", "-aws-security-group", "sg-1", "-aws-group", "prod-stagelet", "-aws-image", "ami-1",
		"-registry", "123456789012.dkr.ecr.eu-central-1.amazonaws.com/prod",
	}
	if err != nil || !reflect.DeepEqual(args, want) || env["AWS_ACCESS_KEY_ID"] != "AKIAEXAMPLE" || env["AWS_SECRET_ACCESS_KEY"] != "secret" {
		t.Fatalf("agent = %v %v %v", args, env, err)
	}
	if _, ok := env["STAGELY_REGISTRY_USERNAME"]; ok {
		t.Fatal("the agent got registry credentials; it must sign in to ECR with its AWS credentials")
	}
	if strings.Contains(strings.Join(args, " "), "secret") {
		t.Fatal("the secret access key is in the agent's arguments, which other users can read")
	}
	if _, _, err := cloud.Agent(map[string]any{}); err == nil {
		t.Fatal("missing outputs were accepted")
	}
	a.GoldenImage, a.Endpoint = "ami-golden", ""
	golden, err := a.Cloud(fstest.MapFS{})
	if err != nil {
		t.Fatalf("Cloud with a golden image: %v", err)
	}
	args, _, err = golden.Agent(outputs)
	if err != nil || slices.Contains(args, "-aws-ec2-url") || !slices.Equal(args[len(args)-2:], []string{"-aws-golden-image", "ami-golden"}) {
		t.Fatalf("agent args with a golden image on AWS = %v, %v", args, err)
	}
}

func TestAWSCloudNeedsCredentialsAndARegion(t *testing.T) {
	for _, a := range []AWS{{SecretAccessKey: "s", Region: "r"}, {AccessKeyID: "a", Region: "r"}, {AccessKeyID: "a", SecretAccessKey: "s"}} {
		if _, err := a.Cloud(fstest.MapFS{}); err == nil {
			t.Errorf("Cloud(%+v) succeeded", a)
		}
	}
}
