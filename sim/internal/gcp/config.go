package gcp

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"regexp"
	"strings"
)

const (
	defaultAccessToken = "ya29.stagely-sim"
	defaultZones       = "us-central1-a,us-central1-b,us-central1-c,us-central1-f,us-east1-b,us-east1-c,us-east1-d,europe-west1-b,europe-west1-c,europe-west1-d"
	instanceLimit      = 24
)

var (
	projectIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	zonePattern      = regexp.MustCompile(`^([a-z]+-[a-z]+[0-9]+)-[a-z]$`)
)

type config struct {
	enabled      bool
	addr         string
	project      string
	accessTokens []string
	zones        []string
	network      string
	tokenURI     string
}

type repeatedFlag []string

func (r *repeatedFlag) String() string { return strings.Join(*r, " ") }

func (r *repeatedFlag) Set(v string) error {
	*r = append(*r, v)
	return nil
}

func registerFlags(fs *flag.FlagSet, cfg *config, tokens *repeatedFlag, zones *string) {
	fs.BoolVar(&cfg.enabled, "gcp", false, "run the Google Cloud simulation")
	fs.StringVar(&cfg.addr, "gcp-addr", "127.0.0.1:9320", "listen address of the simulated Google Cloud APIs (compute.googleapis.com, oauth2.googleapis.com, artifactregistry.googleapis.com); instance user data runs as root in privileged containers, so expose it with care")
	fs.StringVar(&cfg.project, "gcp-project", "stagely-sim", "project ID that the simulated APIs serve")
	fs.Var(tokens, "gcp-access-token", "OAuth 2.0 access token that the simulated APIs accept besides the tokens they issue; repeatable (default "+defaultAccessToken+")")
	fs.StringVar(zones, "gcp-zones", defaultZones, "comma-separated zones; their regions are the region part of each name")
	fs.StringVar(&cfg.network, "gcp-network", "stagely-sim-gcp", "Docker network for instances' internal addresses; external addresses use <network>-public")
	fs.StringVar(&cfg.tokenURI, "gcp-token-uri", "", "token_uri written into the service account key file (default http://<gcp-addr>/token)")
}

func (c *config) finish(tokens repeatedFlag, zones string) error {
	c.accessTokens = tokens
	if len(c.accessTokens) == 0 {
		c.accessTokens = []string{defaultAccessToken}
	}
	if !projectIDPattern.MatchString(c.project) {
		return fmt.Errorf("gcp-project %q is not a valid project ID", c.project)
	}
	for _, z := range strings.Split(zones, ",") {
		z = strings.TrimSpace(z)
		if z == "" {
			continue
		}
		if !zonePattern.MatchString(z) {
			return fmt.Errorf("gcp-zones: %q is not a zone name such as us-central1-a", z)
		}
		c.zones = append(c.zones, z)
	}
	if len(c.zones) == 0 {
		return errors.New("gcp-zones must name at least one zone")
	}
	if c.network == "" {
		return errors.New("gcp-network must not be empty")
	}
	if c.tokenURI == "" {
		host, port, err := net.SplitHostPort(c.addr)
		if err != nil {
			return fmt.Errorf("gcp-addr: %w", err)
		}
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "localhost"
		}
		c.tokenURI = "http://" + net.JoinHostPort(host, port) + "/token"
	}
	return nil
}

// regionOf returns the region of a zone name, such as us-central1 for
// us-central1-a.
func regionOf(zone string) string {
	if m := zonePattern.FindStringSubmatch(zone); m != nil {
		return m[1]
	}
	return ""
}
