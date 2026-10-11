package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stagely-dev/stagely/internal/agent/build"
	"github.com/stagely-dev/stagely/internal/agent/journal"
	"github.com/stagely-dev/stagely/protocol"
)

const (
	commit   = "0123456789abcdef0123456789abcdef01234567"
	registry = "registry.example.com/acme"
	resolved = `{"name":"stagelet","services":{"web":{"build":{"context":"/var/lib/stagely/builds/5e1a9c0b7d42/src"},"environment":{"MODE":"dev"}}}}`
)

// fakeMachines is a cloud whose machines answer commands from a script.
type fakeMachines struct {
	mu        sync.Mutex
	existing  map[string]bool
	address   string
	createErr error
	created   []string
	deleted   []string
	commands  []string
	// reply returns what a command prints and whether it fails.
	reply func(machine, command string) (stdout string, err error)
	// block makes commands that contain it wait until the context ends.
	block string
}

func newFakeMachines(address string) *fakeMachines {
	ref := registry + "/acme/web/web"
	return &fakeMachines{existing: map[string]bool{}, address: address, reply: func(_, command string) (string, error) {
		switch {
		case strings.HasSuffix(command, "config --format json"):
			return resolved, nil
		case strings.Contains(command, "image inspect"):
			return `["` + ref + `@sha256:feed"]`, nil
		case strings.Contains(command, "curl -s -o /dev/null"):
			return "200", nil
		}
		return "", nil
	}}
}

func (f *fakeMachines) Exists(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.existing[name], nil
}

func (f *fakeMachines) Create(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return f.createErr
	}
	f.created = append(f.created, name)
	f.existing[name] = true
	return nil
}

func (f *fakeMachines) Address(context.Context, string) (string, error) { return f.address, nil }

func (f *fakeMachines) Run(ctx context.Context, name string, stdin io.Reader, stdout, _ io.Writer, args ...string) error {
	if stdin != nil {
		_, _ = io.Copy(io.Discard, stdin)
	}
	command := strings.Join(args, " ")
	f.mu.Lock()
	f.commands = append(f.commands, name+": "+command)
	block, reply := f.block, f.reply
	f.mu.Unlock()
	if block != "" && strings.Contains(command, block) {
		<-ctx.Done()
		return ctx.Err()
	}
	out, err := reply(name, command)
	if stdout != nil {
		_, _ = io.WriteString(stdout, out)
	}
	return err
}

func (f *fakeMachines) CopyIn(context.Context, string, string, string) error { return nil }

func (f *fakeMachines) Delete(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, name)
	delete(f.existing, name)
	return nil
}

func (f *fakeMachines) List(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for name := range f.existing {
		names = append(names, name)
	}
	return names, nil
}

func (f *fakeMachines) ran(substr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.ContainsFunc(f.commands, func(c string) bool { return strings.Contains(c, substr) })
}

type sent struct {
	msgType string
	payload any
}

// fakeSession is one control plane session. Close ends it.
type fakeSession struct {
	in     chan protocol.Envelope
	out    chan sent
	closed chan struct{}
	once   sync.Once
}

func newFakeSession() *fakeSession {
	return &fakeSession{in: make(chan protocol.Envelope, 8), out: make(chan sent, 1000), closed: make(chan struct{})}
}

func (s *fakeSession) Receive(ctx context.Context) (protocol.Envelope, error) {
	select {
	case env := <-s.in:
		return env, nil
	case <-s.closed:
		return protocol.Envelope{}, errors.New("session closed")
	case <-ctx.Done():
		return protocol.Envelope{}, ctx.Err()
	}
}

func (s *fakeSession) Send(_ context.Context, msgType string, payload any) error {
	select {
	case <-s.closed:
		return errors.New("session closed")
	default:
	}
	s.out <- sent{msgType, payload}
	return nil
}

func (s *fakeSession) Close() { s.once.Do(func() { close(s.closed) }) }

func (s *fakeSession) push(t *testing.T, msgType string, payload any) {
	t.Helper()
	data, err := protocol.Encode(msgType, payload)
	if err != nil {
		t.Fatalf("encode %s: %v", msgType, err)
	}
	env, err := protocol.Decode(data)
	if err != nil {
		t.Fatalf("decode %s: %v", msgType, err)
	}
	s.in <- env
}

// await returns the next message of msgType, and every message sent before
// it.
func (s *fakeSession) await(t *testing.T, msgType string) (any, []sent) {
	t.Helper()
	var before []sent
	timeout := time.After(10 * time.Second)
	for {
		select {
		case m := <-s.out:
			if m.msgType == msgType {
				return m.payload, before
			}
			before = append(before, m)
		case <-timeout:
			t.Fatalf("no %s message within 10s; got %v", msgType, before)
			return nil, nil
		}
	}
}

type fakeSecrets map[string]string

func (f fakeSecrets) Resolve(keys []string) (map[string]string, error) {
	out := map[string]string{}
	for _, k := range keys {
		v, ok := f[k]
		if !ok {
			return nil, fmt.Errorf("the agent has no secret %q", k)
		}
		out[k] = v
	}
	return out, nil
}

func healthyApp(t *testing.T) (host string, port int) {
	t.Helper()
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/web/tarball/" + commit:
			_, _ = io.WriteString(w, "tarball")
		case "/healthz":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(app.Close)
	u, _ := url.Parse(app.URL)
	h, p, _ := net.SplitHostPort(u.Host)
	port, _ = strconv.Atoi(p)
	return h, port
}

func newRunner(t *testing.T, machines Machines) *Runner {
	t.Helper()
	return newRunnerWithJournal(t, machines, openJournal(t, filepath.Join(t.TempDir(), "journal.db")))
}

func newRunnerWithJournal(t *testing.T, machines Machines, j *journal.Journal) *Runner {
	t.Helper()
	return &Runner{
		Machines:       machines,
		Journal:        j,
		Source:         &build.Source{HTTP: http.DefaultClient, MaxBytes: 1 << 20},
		Secrets:        fakeSecrets{"api": "s3cr3t-token-value"},
		Registry:       Registry{Prefix: registry, Credentials: StaticCredentials("registry-user-token", "registry-password")},
		MachinePrefix:  "stagely-",
		BuildWorkers:   1,
		MaxJobs:        2,
		RenewEvery:     time.Hour,
		HealthTimeout:  5 * time.Second,
		InventoryEvery: time.Hour,
		Log:            slog.New(slog.DiscardHandler),
	}
}

func openJournal(t *testing.T, path string) *journal.Journal {
	t.Helper()
	j, err := journal.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

// serve runs the runner on a new session until the test ends.
func serve(t *testing.T, r *Runner) *fakeSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := newFakeSession()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.Serve(ctx, s)
	}()
	t.Cleanup(func() {
		cancel()
		s.Close()
		<-done
		r.Wait()
	})
	return s
}

func deployJob(port int, secrets ...protocol.SecretRef) protocol.Job {
	return protocol.Job{ID: "op1", LeaseToken: "lt1", Kind: protocol.JobDeploy, StageletID: "5e1a9c0b7d42", Deploy: &protocol.DeploySpec{
		Repository: "acme/web", Commit: commit, SourceAPIURL: "http://127.0.0.1:" + strconv.Itoa(port), ComposeFiles: []string{"compose.yml"}, PublicService: "web", PublicPort: port,
		HealthPath: "/healthz", Config: map[string]string{"MODE": "preview"}, Secrets: secrets,
	}}
}

func steps(msgs []sent) []string {
	var out []string
	for _, m := range msgs {
		if st, ok := m.payload.(protocol.JobStep); ok {
			out = append(out, st.Step+":"+st.State)
		}
	}
	return out
}

func TestDeployBuildsOnAWorkerAndRunsTheBundleByDigest(t *testing.T) {
	host, port := healthyApp(t)
	machines := newFakeMachines(host)
	r := newRunner(t, machines)
	s := serve(t, r)

	s.push(t, protocol.TypeJob, deployJob(port))
	payload, before := s.await(t, protocol.TypeJobResult)
	res := payload.(protocol.JobResult)

	if !res.OK || res.Upstream != net.JoinHostPort(host, strconv.Itoa(port)) {
		t.Fatalf("result = %+v, want OK with the machine's upstream", res)
	}
	digest := registry + "/acme/web/web@sha256:feed"
	if res.Images["web"] != digest || !strings.Contains(string(res.Bundle), `"image":"`+digest+`"`) || !strings.Contains(string(res.Bundle), `"MODE":"preview"`) {
		t.Fatalf("images = %v, bundle = %s; want the web image by digest with the config", res.Images, res.Bundle)
	}
	if !slices.Equal(machines.created, []string{"stagely-build-1", "stagely-5e1a9c0b7d42"}) && !slices.Equal(machines.created, []string{"stagely-5e1a9c0b7d42", "stagely-build-1"}) {
		t.Fatalf("created = %v, want a build worker and the stagelet machine", machines.created)
	}
	if !machines.ran("stagely-build-1: sudo env BUILDKIT_PROGRESS=plain MODE=preview docker compose") || !machines.ran("stagely-5e1a9c0b7d42: sudo sh -c") {
		t.Fatal("the build did not run on the worker, or the bundle did not start on the stagelet machine")
	}
	got := steps(before)
	for _, want := range []string{"build:succeeded", "provision:succeeded", "start:succeeded", "migrate:skipped", "health:succeeded"} {
		if !slices.Contains(got, want) {
			t.Fatalf("steps = %v, want %s", got, want)
		}
	}
}

func TestDeployReturnsABundleThatRunsImageOnlyServicesByDigest(t *testing.T) {
	host, port := healthyApp(t)
	machines := newFakeMachines(host)
	base := machines.reply
	machines.reply = func(machine, command string) (string, error) {
		switch {
		case strings.HasSuffix(command, "config --format json"):
			return `{"services":{"web":{"build":{"context":"/var/lib/stagely/builds/5e1a9c0b7d42/src"}},"db":{"image":"postgres:17"}}}`, nil
		case strings.HasSuffix(command, "image inspect --format {{json .RepoDigests}} postgres:17"):
			return `["postgres@sha256:bb"]`, nil
		}
		return base(machine, command)
	}
	r := newRunner(t, machines)
	s := serve(t, r)

	s.push(t, protocol.TypeJob, deployJob(port))
	payload, _ := s.await(t, protocol.TypeJobResult)
	res := payload.(protocol.JobResult)

	if !res.OK || !strings.Contains(string(res.Bundle), `"image":"postgres@sha256:bb"`) {
		t.Fatalf("result = %+v, bundle = %s; want the db image by digest", res, res.Bundle)
	}
}

func TestDeployOutsideTheComposeSubsetFailsAtBuildAndStartsNothing(t *testing.T) {
	host, port := healthyApp(t)
	machines := newFakeMachines(host)
	base := machines.reply
	machines.reply = func(machine, command string) (string, error) {
		if strings.HasSuffix(command, "config --format json") {
			return `{"services":{"web":{"image":"nginx","volumes":[{"type":"bind","source":"/","target":"/host"}]}}}`, nil
		}
		return base(machine, command)
	}
	r := newRunner(t, machines)
	s := serve(t, r)

	s.push(t, protocol.TypeJob, deployJob(port))
	payload, before := s.await(t, protocol.TypeJobResult)
	res := payload.(protocol.JobResult)

	if res.OK || !strings.Contains(res.Error, `build: the Compose project is outside the supported subset: service web: volumes: bind mount "/" is not allowed`) {
		t.Fatalf("result = %+v, want the build step to name the bind mount", res)
	}
	if !slices.Contains(steps(before), "build:failed") || machines.ran(" up --detach") {
		t.Fatalf("steps = %v, want build:failed and nothing started", steps(before))
	}
}

func TestBuildWorkerSignsInToPushAndStageletMachineToPullOnly(t *testing.T) {
	host, port := healthyApp(t)
	machines := newFakeMachines(host)
	r := newRunner(t, machines)
	var hosts []string
	var mu sync.Mutex
	r.Registry.Credentials = func(_ context.Context, host string, push bool) (string, string, error) {
		mu.Lock()
		defer mu.Unlock()
		hosts = append(hosts, host)
		if push {
			return "pusher", "push-password", nil
		}
		return "puller", "pull-password", nil
	}
	s := serve(t, r)

	s.push(t, protocol.TypeJob, deployJob(port))
	payload, _ := s.await(t, protocol.TypeJobResult)

	if res := payload.(protocol.JobResult); !res.OK {
		t.Fatalf("result = %+v, want OK", res)
	}
	if !machines.ran("stagely-build-1: sudo docker login --username pusher --password-stdin registry.example.com") {
		t.Fatal("the build worker did not sign in with credentials that can push")
	}
	if !machines.ran("stagely-5e1a9c0b7d42: sudo docker login --username puller --password-stdin registry.example.com") {
		t.Fatal("the stagelet machine did not sign in with pull-only credentials")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hosts) != 2 || hosts[0] != "registry.example.com" || hosts[1] != "registry.example.com" {
		t.Fatalf("credentials asked for %v, want the registry host twice", hosts)
	}
}

func TestFailedDeployNamesTheFailedStep(t *testing.T) {
	host, port := healthyApp(t)
	machines := newFakeMachines(host)
	machines.createErr = errors.New("no capacity in fra1")
	r := newRunner(t, machines)
	s := serve(t, r)

	s.push(t, protocol.TypeJob, deployJob(port))
	payload, before := s.await(t, protocol.TypeJobResult)
	res := payload.(protocol.JobResult)

	if res.OK || !strings.Contains(res.Error, "provision: no capacity in fra1") {
		t.Fatalf("result = %+v, want the provision failure", res)
	}
	if !slices.Contains(steps(before), "provision:failed") {
		t.Fatalf("steps = %v, want provision:failed", steps(before))
	}
}

func failComposeUp(machines *fakeMachines) {
	base := machines.reply
	machines.reply = func(machine, command string) (string, error) {
		if strings.Contains(command, " up --detach") {
			return "", errors.New("exit status 1")
		}
		return base(machine, command)
	}
}

func resetJob(port int) protocol.Job {
	return protocol.Job{ID: "op2", LeaseToken: "lt2", Kind: protocol.JobReset, StageletID: "5e1a9c0b7d42", Reset: &protocol.ResetSpec{
		Bundle: json.RawMessage(`{"services":{"web":{"image":"x@sha256:aa"}}}`), PublicService: "web", PublicPort: port, HealthPath: "/healthz",
	}}
}

func TestFailedJobSaysWhetherItChangedTheStageletMachine(t *testing.T) {
	tests := []struct {
		name    string
		job     func(port int) protocol.Job
		breakIt func(*fakeMachines)
		changed bool
	}{
		{
			name: "deploy fails before the machine runs anything", job: func(port int) protocol.Job { return deployJob(port) },
			breakIt: func(m *fakeMachines) { m.createErr = errors.New("no capacity in fra1") },
		},
		{name: "deploy fails while Compose starts the app", job: func(port int) protocol.Job { return deployJob(port) }, breakIt: failComposeUp, changed: true},
		{name: "reset fails because the machine is gone", job: resetJob, breakIt: func(m *fakeMachines) { delete(m.existing, "stagely-5e1a9c0b7d42") }},
		{name: "reset fails while Compose starts the app", job: resetJob, breakIt: failComposeUp, changed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, port := healthyApp(t)
			machines := newFakeMachines(host)
			machines.existing["stagely-5e1a9c0b7d42"] = true
			tt.breakIt(machines)
			s := serve(t, newRunner(t, machines))

			s.push(t, protocol.TypeJob, tt.job(port))
			payload, _ := s.await(t, protocol.TypeJobResult)
			res := payload.(protocol.JobResult)

			if res.OK || res.MachineChanged != tt.changed {
				t.Fatalf("result = %+v, want a failure with machine_changed %v", res, tt.changed)
			}
		})
	}
}

func TestSecretValuesNeverLeaveTheAgent(t *testing.T) {
	host, port := healthyApp(t)
	machines := newFakeMachines(host)
	base := machines.reply
	machines.reply = func(machine, command string) (string, error) {
		if strings.Contains(command, " up --detach") {
			return "token is s3cr3t-token-value, registry login registry-user-token:registry-password\n", errors.New("exit status 1")
		}
		return base(machine, command)
	}
	r := newRunner(t, machines)
	s := serve(t, r)

	s.push(t, protocol.TypeJob, deployJob(port, protocol.SecretRef{Name: "API_TOKEN", Key: "api"}))
	payload, before := s.await(t, protocol.TypeJobResult)

	payloads := []any{payload}
	for _, m := range before {
		payloads = append(payloads, m.payload)
	}
	everything, _ := json.Marshal(payloads)
	if strings.Contains(string(everything), "s3cr3t-token-value") || strings.Contains(string(everything), "registry-password") || strings.Contains(string(everything), "registry-user-token") {
		t.Fatalf("a secret value reached the control plane: %s", everything)
	}
	if !strings.Contains(string(everything), "token is [secret]") {
		t.Fatalf("messages = %s, want the log line with the value hidden", everything)
	}
}

func TestMissingSecretFailsBeforeAnyMachineIsCreated(t *testing.T) {
	host, port := healthyApp(t)
	machines := newFakeMachines(host)
	r := newRunner(t, machines)
	s := serve(t, r)

	s.push(t, protocol.TypeJob, deployJob(port, protocol.SecretRef{Name: "DB_URL", Key: "db"}))
	payload, _ := s.await(t, protocol.TypeJobResult)
	res := payload.(protocol.JobResult)

	if res.OK || !strings.Contains(res.Error, `secrets: the agent has no secret "db"`) {
		t.Fatalf("result = %+v, want the missing key named", res)
	}
	if len(machines.created) != 0 {
		t.Fatalf("created = %v, want no machines", machines.created)
	}
}

func TestResultIsSentAgainAfterTheControlPlaneAdoptsTheJob(t *testing.T) {
	host, port := healthyApp(t)
	machines := newFakeMachines(host)
	r := newRunner(t, machines)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); r.Wait() }()

	first := newFakeSession()
	go func() { _ = r.Serve(ctx, first) }()
	first.await(t, protocol.TypeJobsInProgress)
	first.push(t, protocol.TypeJob, deployJob(port))
	first.await(t, protocol.TypeJobStep)
	first.Close()
	waitFor(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.jobs["op1"] != nil && r.jobs["op1"].result != nil
	})

	second := newFakeSession()
	go func() { _ = r.Serve(ctx, second) }()
	payload, _ := second.await(t, protocol.TypeJobsInProgress)
	if held := payload.(protocol.JobsInProgress).Jobs; len(held) != 1 || held[0].JobID != "op1" || held[0].LeaseToken != "lt1" {
		t.Fatalf("jobs in progress = %v, want op1 under lt1", held)
	}
	second.push(t, protocol.TypeJobAdopted, protocol.JobAdopted{JobID: "op1"})
	result, _ := second.await(t, protocol.TypeJobResult)
	if res := result.(protocol.JobResult); !res.OK || res.JobID != "op1" {
		t.Fatalf("result = %+v, want op1 OK", res)
	}
	second.Close()
}

func TestAbandonedJobStopsAndSendsNoResult(t *testing.T) {
	host, port := healthyApp(t)
	machines := newFakeMachines(host)
	machines.block = " up --detach"
	r := newRunner(t, machines)
	s := serve(t, r)

	s.push(t, protocol.TypeJob, deployJob(port))
	waitFor(t, func() bool { return machines.ran(" up --detach") })
	s.push(t, protocol.TypeJobAbandoned, protocol.JobAbandoned{JobID: "op1", Reason: "lease expired"})

	waitFor(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.jobs) == 0 && len(r.slots) == 0
	})
	for {
		select {
		case m := <-s.out:
			if m.msgType == protocol.TypeJobResult {
				t.Fatalf("result %+v sent for an abandoned job", m.payload)
			}
		default:
			return
		}
	}
}

func TestResetReappliesTheBundleWithoutBuildingOrMigrating(t *testing.T) {
	host, port := healthyApp(t)
	machines := newFakeMachines(host)
	machines.existing["stagely-5e1a9c0b7d42"] = true
	r := newRunner(t, machines)
	s := serve(t, r)

	s.push(t, protocol.TypeJob, resetJob(port))
	payload, before := s.await(t, protocol.TypeJobResult)

	if res := payload.(protocol.JobResult); !res.OK {
		t.Fatalf("result = %+v, want OK", res)
	}
	if !machines.ran("--force-recreate") || machines.ran(" build --pull") || machines.ran(" exec -T") {
		t.Fatal("reset must recreate the containers without building or migrating")
	}
	if got := steps(before); !slices.Equal(got, []string{"start:started", "start:succeeded", "health:started", "health:succeeded"}) {
		t.Fatalf("steps = %v, want start and health only", got)
	}
}

func TestDeleteMachineRefusesAMachineTheAgentDidNotCreate(t *testing.T) {
	machines := newFakeMachines("127.0.0.1")
	machines.existing["customer-db"] = true
	r := newRunner(t, machines)
	s := serve(t, r)

	s.push(t, protocol.TypeJob, protocol.Job{ID: "op3", LeaseToken: "lt3", Kind: protocol.JobDeleteMachine, Machine: "customer-db"})
	payload, _ := s.await(t, protocol.TypeJobResult)

	if res := payload.(protocol.JobResult); res.OK || len(machines.deleted) != 0 {
		t.Fatalf("result = %+v, deleted = %v; want a refusal and nothing deleted", res, machines.deleted)
	}
}

func TestInventoryListsOwnMachinesAndShrinksThePool(t *testing.T) {
	machines := newFakeMachines("127.0.0.1")
	for _, name := range []string{"stagely-5e1a9c0b7d42", "stagely-build-1", "stagely-build-2", "customer-db", "stagely-Not_Ours", "stagely-gateway"} {
		machines.existing[name] = true
	}
	r := newRunner(t, machines)
	s := serve(t, r)

	payload, _ := s.await(t, protocol.TypeInventory)
	got := payload.(protocol.Inventory).Machines
	want := []protocol.Machine{
		{Name: "stagely-5e1a9c0b7d42", Kind: protocol.MachineRuntime, StageletID: "5e1a9c0b7d42", Status: protocol.MachineReady},
		{Name: "stagely-build-1", Kind: protocol.MachineBuildWorker, Status: protocol.MachineReady},
		{Name: "stagely-build-2", Kind: protocol.MachineBuildWorker, Status: protocol.MachineReady},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("inventory = %+v, want %+v", got, want)
	}
	waitFor(t, func() bool {
		machines.mu.Lock()
		defer machines.mu.Unlock()
		return slices.Equal(machines.deleted, []string{"stagely-build-2"})
	})
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 10s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestFailedCreateDeletesTheHalfCreatedMachine(t *testing.T) {
	host, port := healthyApp(t)
	machines := newFakeMachines(host)
	machines.createErr = errors.New("droplet never became ready")
	r := newRunner(t, machines)
	s := serve(t, r)

	s.push(t, protocol.TypeJob, deployJob(port))
	s.await(t, protocol.TypeJobResult)

	machines.mu.Lock()
	deleted := slices.Clone(machines.deleted)
	machines.mu.Unlock()
	if !slices.Contains(deleted, "stagely-5e1a9c0b7d42") {
		t.Fatalf("deleted = %v, want the half-created stagely-5e1a9c0b7d42 deleted", deleted)
	}
	if pending, _ := r.Journal.Machines(context.Background()); len(pending) != 0 {
		t.Fatalf("journal machines = %v, want none after the cleanup", pending)
	}
}

func TestRestartedAgentDeletesMachinesWithUnfinishedChanges(t *testing.T) {
	ctx := context.Background()
	j := openJournal(t, filepath.Join(t.TempDir(), "journal.db"))
	if err := j.SetMachine(ctx, "stagely-s9", protocol.MachineCreating); err != nil {
		t.Fatal(err)
	}
	machines := newFakeMachines("127.0.0.1")
	machines.existing["stagely-s9"] = true
	r := newRunnerWithJournal(t, machines, j)

	if err := r.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	r.Wait()

	if !slices.Equal(machines.deleted, []string{"stagely-s9"}) {
		t.Fatalf("deleted = %v, want stagely-s9", machines.deleted)
	}
	if pending, _ := j.Machines(ctx); len(pending) != 0 {
		t.Fatalf("journal machines = %v, want none", pending)
	}
}

func TestRestartedAgentRunsAnAdoptedJobAgain(t *testing.T) {
	host, port := healthyApp(t)
	ctx := context.Background()
	j := openJournal(t, filepath.Join(t.TempDir(), "journal.db"))
	spec, _ := json.Marshal(deployJob(port))
	if err := j.StartJob(ctx, "op1", "lt1", spec); err != nil {
		t.Fatal(err)
	}
	machines := newFakeMachines(host)
	r := newRunnerWithJournal(t, machines, j)
	if err := r.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	s := serve(t, r)

	payload, _ := s.await(t, protocol.TypeJobsInProgress)
	if held := payload.(protocol.JobsInProgress).Jobs; len(held) != 1 || held[0] != (protocol.JobRef{JobID: "op1", LeaseToken: "lt1"}) {
		t.Fatalf("jobs in progress = %v, want op1 from the journal", held)
	}
	s.push(t, protocol.TypeJobAdopted, protocol.JobAdopted{JobID: "op1"})
	result, _ := s.await(t, protocol.TypeJobResult)

	if res := result.(protocol.JobResult); !res.OK || res.JobID != "op1" {
		t.Fatalf("result = %+v, want op1 OK", res)
	}
	waitFor(t, func() bool { jobs, _ := j.Jobs(ctx); return len(jobs) == 0 })
}

func TestRestartedAgentDropsAnAbandonedJob(t *testing.T) {
	ctx := context.Background()
	j := openJournal(t, filepath.Join(t.TempDir(), "journal.db"))
	spec, _ := json.Marshal(protocol.Job{ID: "op1", LeaseToken: "lt1", Kind: protocol.JobDelete, StageletID: "5e1a9c0b7d42"})
	if err := j.StartJob(ctx, "op1", "lt1", spec); err != nil {
		t.Fatal(err)
	}
	if err := j.FinishJob(ctx, "op1", []byte(`{"job_id":"op1","lease_token":"lt1","ok":true}`)); err != nil {
		t.Fatal(err)
	}
	machines := newFakeMachines("127.0.0.1")
	r := newRunnerWithJournal(t, machines, j)
	if err := r.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	s := serve(t, r)
	s.await(t, protocol.TypeJobsInProgress)

	s.push(t, protocol.TypeJobAbandoned, protocol.JobAbandoned{JobID: "op1", Reason: "lease expired"})

	waitFor(t, func() bool { jobs, _ := j.Jobs(ctx); return len(jobs) == 0 })
	if len(machines.deleted) != 0 {
		t.Fatalf("deleted = %v, want nothing run for an abandoned job", machines.deleted)
	}
}
