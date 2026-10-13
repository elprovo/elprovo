package digitalocean

import (
	"maps"
	"testing"
)

func TestParsePublishRuleReadsSelectorAndPorts(t *testing.T) {
	rule, err := parsePublishRule("tag=elprovo-gateway:443=8443,80=8081")
	if err != nil {
		t.Fatal(err)
	}
	if rule.tag != "elprovo-gateway" || !maps.Equal(rule.ports, map[int]int{443: 8443, 80: 8081}) {
		t.Fatalf("rule = %+v", rule)
	}
	if !rule.matches("gw-1", []string{"web", "elprovo-gateway"}) || rule.matches("gw-1", []string{"web"}) {
		t.Fatal("tag rule matches the wrong droplets")
	}
	byName, err := parsePublishRule("name=gw-1:22=2222")
	if err != nil || !byName.matches("gw-1", nil) || byName.matches("gw-2", nil) {
		t.Fatalf("name rule = %+v, %v", byName, err)
	}
}

func TestParsePublishRuleRejectsMalformedRules(t *testing.T) {
	for _, value := range []string{"elprovo-gateway:443=8443", "tag=:443=8443", "ip=1.2.3.4:443=8443", "tag=gw:443", "tag=gw:443=99999", "tag=gw"} {
		if _, err := parsePublishRule(value); err == nil {
			t.Errorf("parsePublishRule(%q) succeeded", value)
		}
	}
}

func TestFinishFillsDefaults(t *testing.T) {
	cfg := config{network: "elprovo-sim", registry: registryConfig{endpoint: "localhost:5000"}}
	if err := cfg.finish(nil, nil, " nyc3, fra1 "); err != nil {
		t.Fatal(err)
	}
	if len(cfg.tokens) != 1 || cfg.tokens[0] != defaultToken || len(cfg.regions) != 2 || cfg.regions[1] != "fra1" {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.registry.apiURL != "http://localhost:5000" || cfg.limits.perHour != 5000 || cfg.limits.perMinute != 250 {
		t.Fatalf("config = %+v", cfg)
	}
}
