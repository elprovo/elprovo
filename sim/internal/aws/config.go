package aws

import (
	"errors"
	"flag"
	"fmt"
	"regexp"
	"strings"
)

const (
	defaultAccessKeyID     = "AKIASTAGELYSIMULATE"
	defaultSecretAccessKey = "stagely-sim-secret-access-key"
	defaultRegions         = "us-east-1,us-east-2,us-west-2,eu-west-1,eu-central-1"
	instanceLimit          = 32
)

var accountIDPattern = regexp.MustCompile(`^[0-9]{12}$`)

type config struct {
	enabled         bool
	addr            string
	accessKeyID     string
	secretAccessKey string
	accountID       string
	regions         []string
	network         string
}

func registerFlags(fs *flag.FlagSet, cfg *config, regions *string) {
	fs.BoolVar(&cfg.enabled, "aws", false, "run the AWS simulation (EC2, STS, ECR and the registry's token service)")
	fs.StringVar(&cfg.addr, "aws-addr", "127.0.0.1:9310", "listen address of the simulated AWS endpoints; instance user data runs as root in privileged containers, so expose it with care")
	fs.StringVar(&cfg.accessKeyID, "aws-access-key-id", defaultAccessKeyID, "access key id that requests must be signed with")
	fs.StringVar(&cfg.secretAccessKey, "aws-secret-access-key", defaultSecretAccessKey, "secret access key that requests must be signed with")
	fs.StringVar(&cfg.accountID, "aws-account-id", "", "12-digit account id (default a random one, kept in the state)")
	fs.StringVar(regions, "aws-regions", defaultRegions, "comma-separated region names; each gets a default VPC")
	fs.StringVar(&cfg.network, "aws-network", "stagely-sim-aws", "Docker network for instances' private addresses; public addresses use <network>-public")
}

func (c *config) finish(regions string) error {
	for _, r := range strings.Split(regions, ",") {
		if r = strings.TrimSpace(r); r != "" {
			c.regions = append(c.regions, r)
		}
	}
	switch {
	case len(c.regions) == 0:
		return errors.New("aws-regions must name at least one region")
	case c.accessKeyID == "" || c.secretAccessKey == "":
		return errors.New("aws-access-key-id and aws-secret-access-key must not be empty")
	case c.accountID != "" && !accountIDPattern.MatchString(c.accountID):
		return fmt.Errorf("aws-account-id %q must be 12 digits", c.accountID)
	case c.network == "":
		return errors.New("aws-network must not be empty")
	}
	return nil
}
