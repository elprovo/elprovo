package setup

import (
	"errors"
	"io/fs"
	"maps"
)

// AWS is an AWS account to bootstrap.
type AWS struct {
	AccessKeyID     string
	SecretAccessKey string
	// SessionToken goes with temporary credentials. The agent keeps the
	// same credentials, so it stops working when they expire.
	SessionToken string
	Region       string
	// Endpoint is one URL for the EC2, ECR and STS APIs, such as a
	// simulation. Empty means AWS.
	Endpoint string
	// AvailabilityZone of the environment's subnet; empty lets AWS choose.
	AvailabilityZone string
	// RegistryPrefix is the ECR repository name prefix that build workers
	// push under; empty means the environment's module name.
	RegistryPrefix string
	// GoldenImage is an AMI from images/aws that the agent boots preview
	// machines and build workers from. Empty means the stock Ubuntu AMI
	// with cloud-init.
	GoldenImage string
}

// Cloud returns what setup needs to bootstrap the account with module.
func (a AWS) Cloud(module fs.FS) (Cloud, error) {
	if a.AccessKeyID == "" || a.SecretAccessKey == "" || a.Region == "" {
		return Cloud{}, errors.New("AWS setup needs an access key id, a secret access key and a region")
	}
	vars := map[string]any{"endpoint": a.Endpoint, "region": a.Region}
	if a.AvailabilityZone != "" {
		vars["availability_zone"] = a.AvailabilityZone
	}
	if a.RegistryPrefix != "" {
		vars["registry_prefix"] = a.RegistryPrefix
	}
	tofuEnv := []string{"AWS_ACCESS_KEY_ID=" + a.AccessKeyID, "AWS_SECRET_ACCESS_KEY=" + a.SecretAccessKey}
	agentEnv := map[string]string{"AWS_ACCESS_KEY_ID": a.AccessKeyID, "AWS_SECRET_ACCESS_KEY": a.SecretAccessKey}
	if a.SessionToken != "" {
		tofuEnv = append(tofuEnv, "AWS_SESSION_TOKEN="+a.SessionToken)
		agentEnv["AWS_SESSION_TOKEN"] = a.SessionToken
	}
	return Cloud{
		Provider: "aws",
		Module:   module,
		TofuEnv:  tofuEnv,
		Vars:     vars,
		Agent: func(o map[string]any) ([]string, map[string]string, error) {
			args := []string{"-aws-region", a.Region}
			if a.Endpoint != "" {
				args = append(args, "-aws-ec2-url", a.Endpoint, "-aws-ecr-url", a.Endpoint, "-aws-sts-url", a.Endpoint)
			}
			for _, out := range []struct{ flag, name string }{
				{"-aws-subnet", "subnet_id"},
				{"-aws-security-group", "preview_security_group_id"},
				{"-aws-group", "preview_group"},
				{"-aws-image", "image_id"},
				{"-registry", "registry"},
			} {
				v, err := outputString(o, out.name)
				if err != nil {
					return nil, nil, err
				}
				args = append(args, out.flag, v)
			}
			if a.GoldenImage != "" {
				args = append(args, "-aws-golden-image", a.GoldenImage)
			}
			return args, maps.Clone(agentEnv), nil
		},
	}, nil
}
