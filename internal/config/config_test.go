package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseRepoAppliesDefaultTimeouts(t *testing.T) {
	repo, err := ParseRepo("elprovo.toml", []byte("version = 1\nstart = \"make start\"\nready = \"make ready\"\n"))
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

func TestParseRepoReadsTables(t *testing.T) {
	content := "version = 1\nstart = \"s\"\nready = \"r\"\npass_env = [\"DOCKER_HOST\"]\n\n" +
		"[env]\nPORT = \"8080\"\n\n[timeouts]\nstart = \"20m\"\n"
	repo, err := ParseRepo("elprovo.toml", []byte(content))
	if err != nil {
		t.Fatal(err)
	}
	if repo.Env["PORT"] != "8080" || len(repo.PassEnv) != 1 || repo.PassEnv[0] != "DOCKER_HOST" || repo.StartTimeout != 20*time.Minute {
		t.Errorf("repo is %+v; want env PORT=8080, pass_env [DOCKER_HOST] and a start timeout of 20m", repo)
	}
}

func TestParseRepoRejectsInvalidFiles(t *testing.T) {
	tests := []struct {
		name, content, wantInError string
	}{
		{"unknown field", "version = 1\nstart = \"s\"\nready = \"r\"\nstopp = \"x\"\n", `unknown field "stopp"`},
		{"two unknown fields", "version = 1\nstart = \"s\"\nready = \"r\"\nstopp = \"x\"\nredy = \"y\"\n", `unknown fields "stopp", "redy"`},
		{"unknown nested field", "version = 1\nstart = \"s\"\nready = \"r\"\n[timeouts]\nstrt = \"1m\"\n", `unknown field "timeouts.strt"`},
		{"duplicate field", "version = 1\nstart = \"s\"\nstart = \"t\"\nready = \"r\"\n", "'start' has already been defined"},
		{"env value that is not a string", "version = 1\nstart = \"s\"\nready = \"r\"\nenv = { PORT = 8080 }\n", "env.PORT"},
		{"missing version", "start = \"s\"\nready = \"r\"\n", "version is missing"},
		{"newer version", "version = 2\nstart = \"s\"\nready = \"r\"\n", "version 2 is not supported"},
		{"missing start", "version = 1\nready = \"r\"\n", "start is missing"},
		{"blank ready", "version = 1\nstart = \"s\"\nready = \"  \"\n", "ready is missing"},
		{"env name", "version = 1\nstart = \"s\"\nready = \"r\"\n[env]\nBAD-NAME = \"x\"\n", `env name "BAD-NAME"`},
		{"reserved env name", "version = 1\nstart = \"s\"\nready = \"r\"\npass_env = [\"ELPROVO_RUN_ID\"]\n", "must not start with ELPROVO_"},
		{"duration without unit", "version = 1\nstart = \"s\"\nready = \"r\"\ntimeouts.start = \"10\"\n", "timeouts.start is \"10\""},
		{"duration too long", "version = 1\nstart = \"s\"\nready = \"r\"\ntimeouts.ready = \"61m\"\n", "the limit is 1h"},
		{"zero duration", "version = 1\nstart = \"s\"\nready = \"r\"\ntimeouts.stop = \"0s\"\n", "timeouts.stop is \"0s\""},
		{"not TOML", "version: 1\nstart: s\nready: r\n", "line 1"},
		{"empty file", "# Only a comment.\n", "the file is empty"},
		{"too large", "version = 1\nstart = \"s\"\nready = \"r\"\n#" + strings.Repeat("x", MaxFileSize), "the limit is 65536"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseRepo("elprovo.toml", []byte(tt.content))
			if err == nil || !strings.Contains(err.Error(), tt.wantInError) {
				t.Errorf("error is %v; want one that contains %q", err, tt.wantInError)
			}
		})
	}
}

func TestParseRepoAcceptsTheLongestDuration(t *testing.T) {
	repo, err := ParseRepo("elprovo.toml", []byte("version = 1\nstart = \"s\"\nready = \"r\"\n[timeouts]\nstart = \"1h\"\nready = \"60m\"\nstop = \"3600s\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if repo.StartTimeout != time.Hour || repo.ReadyTimeout != time.Hour || repo.StopTimeout != time.Hour {
		t.Errorf("timeouts are %v, %v, %v; want 1h each", repo.StartTimeout, repo.ReadyTimeout, repo.StopTimeout)
	}
}

func TestParseProofDefaults(t *testing.T) {
	proof, err := ParseProof("proofs/coupon-once.toml", []byte("claim = \"A coupon applies once.\"\nrun = \"./check\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if proof.Name != "coupon-once" || proof.Before != BeforeFail || proof.Timeout != 10*time.Minute {
		t.Errorf("proof is %+v; want name coupon-once, before fail, timeout 10m", proof)
	}
}

func TestParseProofKeepsTextAsWritten(t *testing.T) {
	tests := []struct {
		name, content, wantClaim, wantRun string
	}{
		{"negated command", "claim = \"c\"\nrun = \"! grep -q RemoveAll syncer.go\"\n", "c", "! grep -q RemoveAll syncer.go"},
		{"test command", "claim = \"c\"\nrun = \"[ -f bin/lnk ]\"\n", "c", "[ -f bin/lnk ]"},
		{"hash and colon in a claim", "claim = \"Fix: issue #58 is fixed.\"\nrun = \"r\"\n", "Fix: issue #58 is fixed.", "r"},
		{"hash in a command", "claim = \"c\"\nrun = \"grep -q ' #' notes.txt\"\n", "c", "grep -q ' #' notes.txt"},
		{"multi-line command", "claim = \"c\"\nrun = '''\ngo build ./...\ngo test ./...\n'''\n", "c", "go build ./...\ngo test ./...\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proof, err := ParseProof("proofs/a.toml", []byte(tt.content))
			if err != nil {
				t.Fatal(err)
			}
			if proof.Claim != tt.wantClaim || proof.Run != tt.wantRun {
				t.Errorf("claim is %q and run is %q; want %q and %q", proof.Claim, proof.Run, tt.wantClaim, tt.wantRun)
			}
		})
	}
}

func TestParseProofRejectsInvalidFiles(t *testing.T) {
	tests := []struct {
		name, file, content, wantInError string
	}{
		{"long claim", "proofs/a.toml", "claim = \"" + strings.Repeat("x", 201) + "\"\nrun = \"r\"\n", "claim has 201 characters"},
		{"two-line claim", "proofs/a.toml", "claim = '''\none\ntwo\n'''\nrun = \"r\"\n", "claim must be one line"},
		{"missing run", "proofs/a.toml", "claim = \"c\"\n", "run is missing"},
		{"unknown before", "proofs/a.toml", "claim = \"c\"\nrun = \"r\"\nbefore = \"pass\"\n", `before is "pass"`},
		{"unknown field", "proofs/a.toml", "claim = \"c\"\nrun = \"r\"\ntimout = \"5m\"\n", `unknown field "timout"`},
		{"duplicate field", "proofs/a.toml", "claim = \"c\"\nrun = \"r\"\nrun = \"s\"\n", "'run' has already been defined"},
		{"invalid name", "proofs/A.toml", "claim = \"c\"\nrun = \"r\"\n", "must match [a-z0-9][a-z0-9-]*"},
		{"not in proofs", "proofs/sub/a.toml", "claim = \"c\"\nrun = \"r\"\n", "must be directly in proofs/"},
		{"YAML proof file", "proofs/a.yml", "claim: c\nrun: r\n", "Elprovo reads proof files that end in .toml"},
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
		{"proofs/coupon-once.toml", "coupon-once", true, false},
		{"proofs/lib.sh", "", false, false},
		{"proofs/sub/a.toml", "", false, false},
		{"proofs/sub/compose.yml", "", false, false},
		{"src/proofs/a.toml", "", false, false},
		{"proofs/Coupon.toml", "", false, true},
		{"proofs/a.yml", "", false, true},
		{"proofs/a.yaml", "", false, true},
	}
	for _, tt := range tests {
		name, proof, err := ProofName(tt.file)
		if name != tt.wantName || proof != tt.wantProof || (err != nil) != tt.wantErr {
			t.Errorf("ProofName(%q) = %q, %v, %v; want %q, %v, error %v", tt.file, name, proof, err, tt.wantName, tt.wantProof, tt.wantErr)
		}
	}
}
