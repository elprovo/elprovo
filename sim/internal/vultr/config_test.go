package vultr

import "testing"

func TestFinishFillsDefaults(t *testing.T) {
	cfg := config{network: "stagely-sim-vultr"}

	if err := cfg.finish(nil); err != nil {
		t.Fatal(err)
	}

	if len(cfg.apiKeys) != 1 || cfg.apiKeys[0] != defaultAPIKey || len(defaultAPIKey) != 36 || cfg.rateLimit != 30 {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestFinishNeedsBothTLSFiles(t *testing.T) {
	cfg := config{network: "stagely-sim-vultr", tlsCert: "api.crt"}

	if err := cfg.finish(nil); err == nil {
		t.Fatal("a certificate without a key was accepted")
	}
}
