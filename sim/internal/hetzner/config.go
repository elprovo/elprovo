package hetzner

import (
	"errors"
	"flag"
	"fmt"
	"strings"
)

// Hetzner API tokens are 64 characters long, and the Terraform provider
// refuses any other length, so the default token has that length too.
const tokenLength = 64

var defaultToken = "stagely-sim-hcloud-" + strings.Repeat("0", tokenLength-len("stagely-sim-hcloud-"))

type config struct {
	enabled   bool
	addr      string
	tokens    []string
	network   string
	rateLimit int
}

type repeatedFlag []string

func (r *repeatedFlag) String() string { return strings.Join(*r, " ") }

func (r *repeatedFlag) Set(v string) error {
	*r = append(*r, v)
	return nil
}

func registerFlags(fs *flag.FlagSet, cfg *config, tokens *repeatedFlag) {
	fs.BoolVar(&cfg.enabled, "hetzner", false, "run the Hetzner Cloud simulation")
	fs.StringVar(&cfg.addr, "hcloud-addr", "127.0.0.1:9400", "listen address of the simulated Hetzner Cloud API (api.hetzner.cloud); server user data runs as root in privileged containers, so expose it with care")
	fs.Var(tokens, "hcloud-token", "64-character API token that the simulated Hetzner Cloud API accepts; repeatable (default "+defaultToken+")")
	fs.StringVar(&cfg.network, "hcloud-network", "stagely-sim-hetzner", "Docker network for servers' private addresses; public addresses use <network>-public")
}

func (c *config) finish(tokens repeatedFlag) error {
	c.tokens = tokens
	if len(c.tokens) == 0 {
		c.tokens = []string{defaultToken}
	}
	for _, t := range c.tokens {
		if len(t) != tokenLength {
			return fmt.Errorf("hcloud-token must be %d characters long, as Hetzner's tokens are; one has %d", tokenLength, len(t))
		}
	}
	if c.network == "" {
		return errors.New("hcloud-network must not be empty")
	}
	c.rateLimit = 3600
	return nil
}
