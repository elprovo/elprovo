package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elprovo/elprovo/protocol"
)

const previewMachine = "elprovo-5e1a9c0b7d42"

// logSignal is a log handler that closes ready on the first record whose
// message contains want.
type logSignal struct {
	want  string
	ready chan struct{}
	once  *sync.Once
}

func (h logSignal) Enabled(context.Context, slog.Level) bool { return true }
func (h logSignal) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h logSignal) WithGroup(string) slog.Handler            { return h }

func (h logSignal) Handle(_ context.Context, rec slog.Record) error {
	if strings.Contains(rec.Message, h.want) {
		h.once.Do(func() { close(h.ready) })
	}
	return nil
}

// onLog makes r's log close the returned channel when r logs want.
func onLog(r *Runner, want string) <-chan struct{} {
	ready := make(chan struct{})
	r.Log = slog.New(logSignal{want: want, ready: ready, once: &sync.Once{}})
	return ready
}

func awaitSignal(t *testing.T, ready <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting until %s", what)
	}
}

// exclusiveMachines counts the commands running on the preview's machine at
// once, and holds the first `compose up` there until its job is cancelled.
type exclusiveMachines struct {
	*fakeMachines
	mu        sync.Mutex
	inFlight  int
	most      int
	held      bool
	upStarted chan struct{}
}

func (m *exclusiveMachines) Run(ctx context.Context, name string, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	if name != previewMachine {
		return m.fakeMachines.Run(ctx, name, stdin, stdout, stderr, args...)
	}
	m.mu.Lock()
	m.inFlight++
	m.most = max(m.most, m.inFlight)
	hold := !m.held && strings.Contains(strings.Join(args, " "), " up --detach")
	m.held = m.held || hold
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.inFlight--
		m.mu.Unlock()
	}()
	if hold {
		close(m.upStarted)
		<-ctx.Done()
		return ctx.Err()
	}
	return m.fakeMachines.Run(ctx, name, stdin, stdout, stderr, args...)
}

func TestANewerJobForAPreviewWaitsUntilTheStaleOneReturns(t *testing.T) {
	host, port := healthyApp(t)
	machines := &exclusiveMachines{fakeMachines: newFakeMachines(host), upStarted: make(chan struct{})}
	r := newRunner(t, machines)
	waiting := onLog(r, "earlier job")
	s := serve(t, r)
	s.push(t, protocol.TypeJob, deployJob(port))
	awaitSignal(t, machines.upStarted, "the first deploy starts its project")

	s.push(t, protocol.TypeJob, protocol.Job{ID: "op2", LeaseToken: "lt2", Kind: protocol.JobReset, PreviewID: "5e1a9c0b7d42", Reset: &protocol.ResetSpec{
		Bundle: json.RawMessage(`{"services":{"web":{"image":"registry.example.com/acme/web/web@sha256:aa"}}}`), PublicService: "web", PublicPort: port, HealthPath: "/healthz",
	}})
	awaitSignal(t, waiting, "the reset waits for the deploy")
	s.push(t, protocol.TypeJobAbandoned, protocol.JobAbandoned{JobID: "op1", Reason: "lease expired"})
	payload, _ := s.await(t, protocol.TypeJobResult)

	if res := payload.(protocol.JobResult); res.JobID != "op2" || !res.OK {
		t.Fatalf("result = %+v, want the reset to succeed once the abandoned deploy returned", res)
	}
	machines.mu.Lock()
	defer machines.mu.Unlock()
	if machines.most != 1 {
		t.Fatalf("%d commands ran on %s at once, want the jobs one after another", machines.most, previewMachine)
	}
}

// slowDeleteMachines holds Delete until release closes, and records any
// command or create on the machine while its delete runs.
type slowDeleteMachines struct {
	*fakeMachines
	deleting chan struct{}
	release  chan struct{}

	mu         sync.Mutex
	inDelete   bool
	duringWork []string
}

func (m *slowDeleteMachines) Delete(ctx context.Context, name string) error {
	m.mu.Lock()
	m.inDelete = true
	m.mu.Unlock()
	close(m.deleting)
	<-m.release
	err := m.fakeMachines.Delete(ctx, name)
	m.mu.Lock()
	m.inDelete = false
	m.mu.Unlock()
	return err
}

func (m *slowDeleteMachines) noteWork(what string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inDelete {
		m.duringWork = append(m.duringWork, what)
	}
}

func (m *slowDeleteMachines) Create(ctx context.Context, name string) error {
	m.noteWork("create " + name)
	return m.fakeMachines.Create(ctx, name)
}

func (m *slowDeleteMachines) Run(ctx context.Context, name string, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	m.noteWork(name + ": " + strings.Join(args, " "))
	return m.fakeMachines.Run(ctx, name, stdin, stdout, stderr, args...)
}

func TestAnAdoptedJobWaitsForTheMachinesARestartInterruptedToBeDeleted(t *testing.T) {
	host, port := healthyApp(t)
	ctx := context.Background()
	j := openJournal(t, filepath.Join(t.TempDir(), "journal.db"))
	if err := j.SetMachine(ctx, previewMachine, protocol.MachineCreating); err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(deployJob(port))
	if err := j.StartJob(ctx, "op1", "lt1", spec); err != nil {
		t.Fatal(err)
	}
	fm := newFakeMachines(host)
	fm.existing[previewMachine] = true
	machines := &slowDeleteMachines{fakeMachines: fm, deleting: make(chan struct{}), release: make(chan struct{})}
	releaseDelete := sync.OnceFunc(func() { close(machines.release) })
	t.Cleanup(releaseDelete)
	r := newRunnerWithJournal(t, machines, j)
	waiting := onLog(r, "a restart interrupted")
	if err := r.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	awaitSignal(t, machines.deleting, "recovery starts deleting the machine")
	s := serve(t, r)
	s.await(t, protocol.TypeJobsInProgress)

	s.push(t, protocol.TypeJobAdopted, protocol.JobAdopted{JobID: "op1"})
	awaitSignal(t, waiting, "the adopted job waits for recovery")
	releaseDelete()
	payload, _ := s.await(t, protocol.TypeJobResult)

	if res := payload.(protocol.JobResult); !res.OK {
		t.Fatalf("result = %+v, want the adopted deploy to succeed on a machine created again", res)
	}
	machines.mu.Lock()
	defer machines.mu.Unlock()
	if len(machines.duringWork) != 0 {
		t.Fatalf("while recovery deleted %s the job did %q, want nothing", previewMachine, machines.duringWork)
	}
	if exists, _ := fm.Exists(ctx, previewMachine); !exists {
		t.Fatal("the machine is gone after the deploy, want it created again")
	}
}
