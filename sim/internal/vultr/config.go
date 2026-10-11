package vultr

import (
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"strings"
)

// defaultAPIKey has the length and alphabet of a Vultr API key.
const defaultAPIKey = "STAGELYSIMVULTRAPIKEY000000000000001"

type config struct {
	enabled  bool
	addr     string
	apiKeys  []string
	network  string
	tlsCert  string
	tlsKey   string
	registry registryConfig
	// rateLimit is the requests a client address may send in one second.
	rateLimit int
}

type registryConfig struct {
	username string
	password string
}

type repeatedFlag []string

func (r *repeatedFlag) String() string { return strings.Join(*r, " ") }

func (r *repeatedFlag) Set(v string) error {
	*r = append(*r, v)
	return nil
}

func registerFlags(fs *flag.FlagSet, cfg *config, apiKeys *repeatedFlag) {
	fs.BoolVar(&cfg.enabled, "vultr", false, "run the Vultr simulation")
	fs.StringVar(&cfg.addr, "vultr-addr", "127.0.0.1:9400", "listen address of the simulated Vultr API (api.vultr.com); instance user data runs as root in privileged containers, so expose it with care")
	fs.Var(apiKeys, "vultr-api-key", "API key that the simulated Vultr API accepts; repeatable (default "+defaultAPIKey+")")
	fs.StringVar(&cfg.network, "vultr-network", "stagely-sim-vultr", "Docker network for instances' private addresses; public addresses use <network>-public")
	fs.StringVar(&cfg.tlsCert, "vultr-tls-cert", "", "PEM certificate to serve the API over HTTPS with, such as one for api.vultr.com; needs -vultr-tls-key")
	fs.StringVar(&cfg.tlsKey, "vultr-tls-key", "", "PEM private key of -vultr-tls-cert")
	fs.StringVar(&cfg.registry.username, "vultr-registry-username", "stagely", "user name of every container registry's root user, which the registry the operator runs must accept")
	fs.StringVar(&cfg.registry.password, "vultr-registry-password", "stagely", "password of every container registry's root user")
}

func (c *config) finish(apiKeys repeatedFlag) error {
	c.apiKeys = apiKeys
	if len(c.apiKeys) == 0 {
		c.apiKeys = []string{defaultAPIKey}
	}
	if c.network == "" {
		return errors.New("vultr-network must not be empty")
	}
	if (c.tlsCert == "") != (c.tlsKey == "") {
		return errors.New("set both -vultr-tls-cert and -vultr-tls-key, or neither")
	}
	if c.tlsCert != "" {
		if _, err := tls.LoadX509KeyPair(c.tlsCert, c.tlsKey); err != nil {
			return fmt.Errorf("-vultr-tls-cert: %w", err)
		}
	}
	c.rateLimit = 30
	return nil
}
