package digitalocean

import (
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"
)

const (
	defaultToken   = "dop_v1_elprovo_sim"
	defaultRegions = "nyc1,nyc3,sfo3,ams3,fra1,lon1,sgp1,tor1,blr1,syd1"
	dropletLimit   = 25
)

type config struct {
	enabled  bool
	addr     string
	tokens   []string
	regions  []string
	network  string
	publish  []publishRule
	registry registryConfig
	limits   rateLimits
}

type registryConfig struct {
	endpoint string
	apiURL   string
	username string
	password string
}

type rateLimits struct {
	perHour   int
	perMinute int
}

type publishRule struct {
	tag   string
	name  string
	ports map[int]int
}

func (r publishRule) matches(name string, tags []string) bool {
	if r.name != "" {
		return r.name == name
	}
	for _, t := range tags {
		if t == r.tag {
			return true
		}
	}
	return false
}

type repeatedFlag []string

func (r *repeatedFlag) String() string { return strings.Join(*r, " ") }

func (r *repeatedFlag) Set(v string) error {
	*r = append(*r, v)
	return nil
}

func registerFlags(fs *flag.FlagSet, cfg *config, tokens, publish *repeatedFlag, regions *string) {
	fs.BoolVar(&cfg.enabled, "digitalocean", false, "run the DigitalOcean simulation")
	fs.StringVar(&cfg.addr, "do-addr", "127.0.0.1:9300", "listen address of the simulated DigitalOcean API (api.digitalocean.com); droplet user data runs as root in privileged containers, so expose it with care")
	fs.Var(tokens, "do-token", "API token that the simulated DigitalOcean API accepts; repeatable (default "+defaultToken+")")
	fs.StringVar(regions, "do-regions", defaultRegions, "comma-separated region slugs; each gets a default VPC")
	fs.StringVar(&cfg.network, "do-network", "elprovo-sim", "Docker network for droplets' private addresses; public addresses use <network>-public")
	fs.Var(publish, "do-publish", "publish ports of matching droplets on 127.0.0.1 of the Docker host, as tag=<tag>:<port>=<host port>,... or name=<name>:...; repeatable")
	fs.StringVar(&cfg.registry.endpoint, "do-registry-endpoint", "localhost:5000", "host:port of the container registry that docker credentials name")
	fs.StringVar(&cfg.registry.apiURL, "do-registry-api", "", "base URL the simulation uses to list repositories (default http://<registry endpoint>)")
	fs.StringVar(&cfg.registry.username, "do-registry-username", "elprovo", "basic-auth user of the container registry")
	fs.StringVar(&cfg.registry.password, "do-registry-password", "elprovo", "basic-auth password of the container registry")
}

func (c *config) finish(tokens, publish repeatedFlag, regions string) error {
	c.tokens = tokens
	if len(c.tokens) == 0 {
		c.tokens = []string{defaultToken}
	}
	for _, r := range strings.Split(regions, ",") {
		if r = strings.TrimSpace(r); r != "" {
			c.regions = append(c.regions, r)
		}
	}
	if len(c.regions) == 0 {
		return errors.New("do-regions must name at least one region")
	}
	for _, p := range publish {
		rule, err := parsePublishRule(p)
		if err != nil {
			return err
		}
		c.publish = append(c.publish, rule)
	}
	if c.registry.apiURL == "" {
		c.registry.apiURL = "http://" + c.registry.endpoint
	}
	c.registry.apiURL = strings.TrimSuffix(c.registry.apiURL, "/")
	c.limits = rateLimits{perHour: 5000, perMinute: 250}
	if c.network == "" {
		return errors.New("do-network must not be empty")
	}
	return nil
}

func parsePublishRule(value string) (publishRule, error) {
	selector, mappings, ok := strings.Cut(value, ":")
	key, match, hasKey := strings.Cut(selector, "=")
	if !ok || !hasKey || match == "" || (key != "tag" && key != "name") {
		return publishRule{}, fmt.Errorf("do-publish %q: the format is tag=<tag>:<port>=<host port>[,<port>=<host port>] or name=<name>:<port>=<host port>", value)
	}
	rule := publishRule{ports: map[int]int{}}
	if key == "tag" {
		rule.tag = match
	} else {
		rule.name = match
	}
	for _, m := range strings.Split(mappings, ",") {
		machine, host, ok := strings.Cut(m, "=")
		mp, err1 := strconv.Atoi(machine)
		hp, err2 := strconv.Atoi(host)
		if !ok || err1 != nil || err2 != nil || mp < 1 || mp > 65535 || hp < 1 || hp > 65535 {
			return publishRule{}, fmt.Errorf("do-publish %q: %q is not <port>=<host port>", value, m)
		}
		rule.ports[mp] = hp
	}
	return rule, nil
}
