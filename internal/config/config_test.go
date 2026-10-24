package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseRepoAppliesDefaultTimeouts(t *testing.T) {
	repo, err := ParseRepo("elprovo.yml", []byte("version: 1\nstart: make start\nready: make ready\n"))
	if err != nil {
		t.Fatal(err)
	}
	if repo.StartTimeout != 10*time.Minute || repo.ReadyTimeout != 5*time.Minute || repo.StopTimeout != 2*time.Minute {
		t.Errorf("timeouts are %v, %v, %v; want 10m, 5m, 2m", repo.StartTimeout, repo.ReadyTimeout, repo.StopTimeout)
	}
	if repo.Stop != "" {
		t.Errorf("stop is %q; want empty", repo.Stop)
	}
}

func TestParseRepoTakesScalarValuesAsWritten(t *testing.T) {
	repo, err := ParseRepo("elprovo.yml", []byte("version: 1\nstart: s\nready: r\nenv:\n  PORT: 8080\n  DEBUG: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if repo.Env["PORT"] != "8080" || repo.Env["DEBUG"] != "true" {
		t.Errorf("env is %v; want PORT=8080 and DEBUG=true", repo.Env)
	}
}

func TestParseRepoRejectsInvalidFiles(t *testing.T) {
	tests := []struct {
		name, content, wantInError string
	}{
		{"unknown field", "version: 1\nstart: s\nready: r\nstopp: x\n", "field stopp not found"},
		{"missing version", "start: s\nready: r\n", "version is missing"},
		{"newer version", "version: 2\nstart: s\nready: r\n", "version 2 is not supported"},
		{"missing start", "version: 1\nready: r\n", "start is missing"},
		{"blank ready", "version: 1\nstart: s\nready: '  '\n", "ready is missing"},
		{"env name", "version: 1\nstart: s\nready: r\nenv:\n  BAD-NAME: x\n", `env name "BAD-NAME"`},
		{"reserved env name", "version: 1\nstart: s\nready: r\npass_env: [ELPROVO_RUN_ID]\n", "must not start with ELPROVO_"},
		{"duration without unit", "version: 1\nstart: s\nready: r\ntimeouts:\n  start: \"10\"\n", "timeouts.start is \"10\""},
		{"duration too long", "version: 1\nstart: s\nready: r\ntimeouts:\n  ready: 61m\n", "the limit is 1h"},
		{"zero duration", "version: 1\nstart: s\nready: r\ntimeouts:\n  stop: 0s\n", "timeouts.stop is \"0s\""},
		{"two documents", "version: 1\nstart: s\nready: r\n---\nversion: 1\n", "one YAML document"},
		{"empty file", "", "the file is empty"},
		{"too large", "version: 1\nstart: s\nready: r\n#" + strings.Repeat("x", MaxFileSize), "the limit is 65536"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseRepo("elprovo.yml", []byte(tt.content))
			if err == nil || !strings.Contains(err.Error(), tt.wantInError) {
				t.Errorf("error is %v; want one that contains %q", err, tt.wantInError)
			}
		})
	}
}

func TestParseRepoAcceptsTheLongestDuration(t *testing.T) {
	repo, err := ParseRepo("elprovo.yml", []byte("version: 1\nstart: s\nready: r\ntimeouts:\n  start: 1h\n  ready: 60m\n  stop: 3600s\n"))
	if err != nil {
		t.Fatal(err)
	}
	if repo.StartTimeout != time.Hour || repo.ReadyTimeout != time.Hour || repo.StopTimeout != time.Hour {
		t.Errorf("timeouts are %v, %v, %v; want 1h each", repo.StartTimeout, repo.ReadyTimeout, repo.StopTimeout)
	}
}

func TestParseProofDefaults(t *testing.T) {
	proof, err := ParseProof("proofs/coupon-once.yml", []byte("claim: A coupon applies once.\nrun: ./check\n"))
	if err != nil {
		t.Fatal(err)
	}
	if proof.Name != "coupon-once" || proof.Before != BeforeFail || proof.Timeout != 10*time.Minute {
		t.Errorf("proof is %+v; want name coupon-once, before fail, timeout 10m", proof)
	}
}

func TestParseProofRejectsInvalidFiles(t *testing.T) {
	tests := []struct {
		name, file, content, wantInError string
	}{
		{"long claim", "proofs/a.yml", "claim: " + strings.Repeat("x", 201) + "\nrun: r\n", "claim has 201 characters"},
		{"two-line claim", "proofs/a.yml", "claim: |\n  one\n  two\nrun: r\n", "claim must be one line"},
		{"missing run", "proofs/a.yml", "claim: c\n", "run is missing"},
		{"unknown before", "proofs/a.yml", "claim: c\nrun: r\nbefore: pass\n", `before is "pass"`},
		{"unknown field", "proofs/a.yml", "claim: c\nrun: r\ntimout: 5m\n", "field timout not found"},
		{"invalid name", "proofs/A.yml", "claim: c\nrun: r\n", "must match [a-z0-9][a-z0-9-]*"},
		{"not in proofs", "proofs/sub/a.yml", "claim: c\nrun: r\n", "must be directly in proofs/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseProof(tt.file, []byte(tt.content))
			if err == nil || !strings.Contains(err.Error(), tt.wantInError) {
				t.Errorf("error is %v; want one that contains %q", err, tt.wantInError)
			}
		})
	}
}

func TestProofNameClassifiesPaths(t *testing.T) {
	tests := []struct {
		file      string
		wantName  string
		wantProof bool
		wantErr   bool
	}{
		{"proofs/coupon-once.yml", "coupon-once", true, false},
		{"proofs/lib.sh", "", false, false},
		{"proofs/sub/a.yml", "", false, false},
		{"proofs/a.yaml", "", false, false},
		{"src/proofs/a.yml", "", false, false},
		{"proofs/Coupon.yml", "", false, true},
	}
	for _, tt := range tests {
		name, proof, err := ProofName(tt.file)
		if name != tt.wantName || proof != tt.wantProof || (err != nil) != tt.wantErr {
			t.Errorf("ProofName(%q) = %q, %v, %v; want %q, %v, error %v", tt.file, name, proof, err, tt.wantName, tt.wantProof, tt.wantErr)
		}
	}
}
