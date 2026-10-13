// Package agent runs the jobs that the control plane leases to this
// environment. A deploy builds the commit's images on a build worker from
// the environment's pool and runs the compiled bundle on the preview's own
// machine. The runner reports each step, the job's output and the result
// under the job's lease. Jobs outlive a dropped session and, through the
// journal, a restart: after a reconnect the agent lists them, and the
// control plane adopts or abandons each one.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/elprovo/elprovo/internal/agent/build"
	"github.com/elprovo/elprovo/internal/agent/journal"
	"github.com/elprovo/elprovo/protocol"
)

// Machines manages the machines of one environment by name.
type Machines interface {
	Exists(ctx context.Context, name string) (bool, error)
	Create(ctx context.Context, name string) error
	Address(ctx context.Context, name string) (string, error)
	// Run writes the command's output to stdout and stderr; nil discards it.
	Run(ctx context.Context, name string, stdin io.Reader, stdout, stderr io.Writer, args ...string) error
	CopyIn(ctx context.Context, name, localPath, remotePath string) error
	// Delete removes the machine and confirms it is gone.
	Delete(ctx context.Context, name string) error
	// List returns the names of the machines the provider can see under the
	// agent's tag. It may include machines the agent did not create.
	List(ctx context.Context) ([]string, error)
}

// Session is the agent's connection to the control plane.
type Session interface {
	Receive(ctx context.Context) (protocol.Envelope, error)
	Send(ctx context.Context, msgType string, payload any) error
}

// Secrets resolves secret keys to their values on the agent host.
type Secrets interface {
	Resolve(keys []string) (map[string]string, error)
}

// Registry is where build workers push images and preview machines pull
// them. Prefix names images, such as registry.example.com/acme; the part
// before the first slash is the host that the credentials sign in to.
// Credentials is nil when the registry needs none.
type Registry struct {
	Prefix      string
	Credentials RegistryCredentials
}

// RegistryCredentials returns the user name and password that a machine's
// Docker signs in to host with for one job: credentials that can push for a
// build worker, and pull only, where the cloud allows it, for a preview
// machine. Machines run untrusted code, so a cloud should hand out
// short-lived credentials that reach only the registry.
type RegistryCredentials func(ctx context.Context, host string, push bool) (username, password string, err error)

// StaticCredentials returns the same user name and password for every job.
func StaticCredentials(username, password string) RegistryCredentials {
	return func(context.Context, string, bool) (string, string, error) {
		return username, password, nil
	}
}

// Runner executes leased jobs.
type Runner struct {
	Machines       Machines
	Journal        *journal.Journal
	Source         *build.Source
	Secrets        Secrets
	Registry       Registry
	MachinePrefix  string
	BuildWorkers   int
	MaxJobs        int
	RenewEvery     time.Duration
	HealthTimeout  time.Duration
	InventoryEvery time.Duration
	Log            *slog.Logger

	initOnce sync.Once
	slots    chan struct{}
	pool     *workerPool
	link     link
	wg       sync.WaitGroup

	mu       sync.Mutex
	jobs     map[string]*job
	machines map[string]string
	// turns holds, for each preview or machine, a channel that closes when
	// the job working on it returns, so a newer job never overlaps a stale
	// one after a reconnect.
	turns map[string]chan struct{}
	// recovered closes when Recover has deleted the machines that changes
	// interrupted by a restart left behind.
	recovered chan struct{}
}

// job is a leased job the agent is running, has finished but not yet
// reported, or found in the journal after a restart. A recovered job runs
// again from the start only when the control plane adopts it.
type job struct {
	spec      protocol.Job
	cancel    context.CancelFunc
	result    *protocol.JobResult
	abandoned bool
	recovered bool
}

var (
	previewIDPattern    = regexp.MustCompile(`^[0-9a-f]{12}$`)
	buildWorkerPattern  = regexp.MustCompile(`^build-([1-9][0-9]{0,3})$`)
	errMachineNotOwned  = errors.New("the machine was not created by this agent")
	errUnknownJobKind   = errors.New("unknown job kind")
	errMissingJobFields = errors.New("job is missing its spec")
)

func (r *Runner) init() {
	r.initOnce.Do(func() {
		r.slots = make(chan struct{}, r.MaxJobs)
		r.pool = newWorkerPool(r.MachinePrefix, r.BuildWorkers)
		r.jobs = map[string]*job{}
		r.machines = map[string]string{}
		r.turns = map[string]chan struct{}{}
		r.recovered = make(chan struct{})
		close(r.recovered)
	})
}

// Recover reads the journal before the first session. Jobs it holds wait
// for the control plane to adopt or abandon them. Machines with an
// unfinished create or delete are deleted in the background, because their
// state is unknown; a later deploy creates them again.
func (r *Runner) Recover(ctx context.Context) error {
	r.init()
	jobs, err := r.Journal.Jobs(ctx)
	if err != nil {
		return err
	}
	machines, err := r.Journal.Machines(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	for _, held := range jobs {
		j := &job{recovered: true, cancel: func() {}}
		if err := json.Unmarshal(held.Spec, &j.spec); err != nil {
			r.mu.Unlock()
			return fmt.Errorf("read journaled job %s: %w", held.ID, err)
		}
		if held.Result != nil {
			j.result = &protocol.JobResult{}
			if err := json.Unmarshal(held.Result, j.result); err != nil {
				r.mu.Unlock()
				return fmt.Errorf("read journaled result of job %s: %w", held.ID, err)
			}
		}
		r.jobs[held.ID] = j
	}
	for name, state := range machines {
		r.machines[name] = state
	}
	recovered := make(chan struct{})
	r.recovered = recovered
	r.mu.Unlock()
	r.wg.Go(func() {
		defer close(recovered)
		for name, state := range machines {
			r.Log.Warn("deleting a machine whose last change did not finish", "machine", name, "state", state)
			if err := r.deleteMachine(ctx, name); err != nil {
				r.Log.Error("delete machine left by an interrupted change", "machine", name, "error", err)
			}
		}
	})
	return nil
}

// Serve handles one session until it ends. It first lists the jobs the
// agent still holds, then takes new jobs and adoption answers. Jobs keep
// running when the session ends; ctx ends them.
func (r *Runner) Serve(ctx context.Context, s Session) error {
	r.init()
	r.link.set(s)
	defer r.link.clear(s)
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if err := s.Send(sessionCtx, protocol.TypeJobsInProgress, r.jobsInProgress()); err != nil {
		return err
	}
	go r.reportInventory(sessionCtx, s)

	for {
		env, err := s.Receive(sessionCtx)
		if err != nil {
			return err
		}
		if err := r.handle(ctx, env); err != nil {
			r.Log.Warn("ignoring malformed message", "type", env.Type, "error", err)
		}
	}
}

// Wait blocks until every job has ended. Call it after the context passed
// to Serve is done.
func (r *Runner) Wait() {
	r.wg.Wait()
}

func (r *Runner) handle(ctx context.Context, env protocol.Envelope) error {
	switch env.Type {
	case protocol.TypeJob:
		var j protocol.Job
		if err := env.Into(&j); err != nil {
			return err
		}
		r.startJob(ctx, j)
	case protocol.TypeJobAdopted:
		var m protocol.JobAdopted
		if err := env.Into(&m); err != nil {
			return err
		}
		r.adopt(ctx, m.JobID)
	case protocol.TypeJobAbandoned:
		var m protocol.JobAbandoned
		if err := env.Into(&m); err != nil {
			return err
		}
		r.abandon(ctx, m.JobID, m.Reason)
	default:
		r.Log.Warn("ignoring unexpected message", "type", env.Type)
	}
	return nil
}

func (r *Runner) jobsInProgress() protocol.JobsInProgress {
	r.mu.Lock()
	defer r.mu.Unlock()
	msg := protocol.JobsInProgress{Jobs: make([]protocol.JobRef, 0, len(r.jobs))}
	for _, j := range r.jobs {
		msg.Jobs = append(msg.Jobs, protocol.JobRef{JobID: j.spec.ID, LeaseToken: j.spec.LeaseToken})
	}
	return msg
}

func (r *Runner) startJob(ctx context.Context, spec protocol.Job) {
	r.mu.Lock()
	_, held := r.jobs[spec.ID]
	r.mu.Unlock()
	if held {
		r.Log.Warn("ignoring a job the agent already holds", "job_id", spec.ID)
		return
	}
	if err := r.journalJob(ctx, spec); err != nil {
		r.Log.Error("refusing a job the journal cannot record", "job_id", spec.ID, "error", err)
		result := protocol.JobResult{JobID: spec.ID, LeaseToken: spec.LeaseToken, Error: "the agent could not record the job in its journal"}
		if err := r.link.send(ctx, protocol.TypeJobResult, result); err != nil {
			r.Log.Warn("report refused job", "job_id", spec.ID, "error", err)
		}
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.launch(ctx, &job{spec: spec})
}

func (r *Runner) journalJob(ctx context.Context, spec protocol.Job) error {
	data, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	return r.Journal.StartJob(ctx, spec.ID, spec.LeaseToken, data)
}

// launch runs j in the background. The caller holds r.mu.
func (r *Runner) launch(ctx context.Context, j *job) {
	jobCtx, cancel := context.WithCancel(ctx)
	j.cancel, j.recovered = cancel, false
	r.jobs[j.spec.ID] = j
	r.wg.Go(func() {
		defer cancel()
		r.runJob(jobCtx, j)
	})
}

// adopt lets a held job continue. A finished job's result goes out now, and
// a job recovered from the journal runs again from the start.
func (r *Runner) adopt(ctx context.Context, jobID string) {
	r.mu.Lock()
	j := r.jobs[jobID]
	if j != nil && j.recovered && j.result == nil {
		r.Log.Info("control plane adopted a job from before the restart; running it again", "job_id", jobID)
		r.launch(ctx, j)
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	if j == nil {
		return
	}
	r.Log.Info("control plane adopted job", "job_id", jobID)
	if j.result != nil {
		r.report(ctx, j)
	}
}

// abandon stops a job whose lease the control plane no longer honors.
func (r *Runner) abandon(ctx context.Context, jobID, reason string) {
	r.mu.Lock()
	j := r.jobs[jobID]
	if j != nil {
		j.abandoned = true
		j.cancel()
		delete(r.jobs, jobID)
	}
	r.mu.Unlock()
	if j == nil {
		return
	}
	r.Log.Warn("control plane abandoned job", "job_id", jobID, "reason", reason)
	if err := r.Journal.ForgetJob(ctx, jobID); err != nil {
		r.Log.Warn("forget abandoned job", "job_id", jobID, "error", err)
	}
}

func (r *Runner) runJob(ctx context.Context, j *job) {
	log := r.Log.With("job_id", j.spec.ID, "kind", j.spec.Kind, "preview_id", j.spec.PreviewID)
	stopRenew := r.renewLease(ctx, j.spec, log)
	defer stopRenew()

	release, ok := r.awaitTurn(ctx, j.spec, log)
	if !ok {
		return
	}
	defer release()
	select {
	case r.slots <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-r.slots }()

	log.Info("job started")
	rep := newReporter(r.link.send, j.spec, log)
	rep.start(ctx)
	result := protocol.JobResult{JobID: j.spec.ID, LeaseToken: j.spec.LeaseToken, OK: true}
	if err := r.execute(ctx, j.spec, rep, &result); err != nil {
		result = protocol.JobResult{JobID: j.spec.ID, LeaseToken: j.spec.LeaseToken, Error: rep.redact(err.Error()), MachineChanged: result.MachineChanged}
		log.Warn("job failed", "error", result.Error)
	} else {
		log.Info("job succeeded", "upstream", result.Upstream)
	}
	rep.close(ctx)

	if data, err := json.Marshal(result); err != nil || r.Journal.FinishJob(ctx, j.spec.ID, data) != nil {
		log.Warn("job result not journaled; a restart before it is delivered loses it", "error", err)
	}
	r.mu.Lock()
	j.result = &result
	abandoned := j.abandoned
	r.mu.Unlock()
	if !abandoned {
		r.report(ctx, j)
	}
}

// awaitTurn waits until Recover has deleted the machines that interrupted
// changes left, and until the agent's earlier job for the same preview, or
// for the same machine, has returned with its cleanup done. Then it holds
// the turn until release. It reports false when ctx ends first. The job
// keeps renewing its lease while it waits.
func (r *Runner) awaitTurn(ctx context.Context, spec protocol.Job, log *slog.Logger) (release func(), ok bool) {
	r.mu.Lock()
	recovered := r.recovered
	r.mu.Unlock()
	select {
	case <-recovered:
	default:
		log.Info("job waits until the machines that a restart interrupted are deleted")
		select {
		case <-recovered:
		case <-ctx.Done():
			return nil, false
		}
	}
	key := spec.PreviewID
	if key == "" {
		key = spec.Machine
	}
	if key == "" {
		return func() {}, true
	}
	for {
		r.mu.Lock()
		earlier, busy := r.turns[key]
		if !busy {
			done := make(chan struct{})
			r.turns[key] = done
			r.mu.Unlock()
			return func() {
				r.mu.Lock()
				delete(r.turns, key)
				r.mu.Unlock()
				close(done)
			}, true
		}
		r.mu.Unlock()
		log.Info("job waits until the agent's earlier job on the same preview or machine returns")
		select {
		case <-earlier:
		case <-ctx.Done():
			return nil, false
		}
	}
}

// report sends a finished job's result. When no session takes it, the job
// stays held, and the next session lists it for adoption.
func (r *Runner) report(ctx context.Context, j *job) {
	if err := r.link.send(ctx, protocol.TypeJobResult, *j.result); err != nil {
		r.Log.Warn("job result not delivered; it is sent again after the control plane adopts the job", "job_id", j.spec.ID, "error", err)
		return
	}
	r.mu.Lock()
	if r.jobs[j.spec.ID] == j {
		delete(r.jobs, j.spec.ID)
	}
	r.mu.Unlock()
	if err := r.Journal.ForgetJob(ctx, j.spec.ID); err != nil {
		r.Log.Warn("forget delivered job", "job_id", j.spec.ID, "error", err)
	}
}

func (r *Runner) renewLease(ctx context.Context, job protocol.Job, log *slog.Logger) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(r.RenewEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := r.link.send(ctx, protocol.TypeLeaseRenew, protocol.LeaseRenew{JobID: job.ID, LeaseToken: job.LeaseToken}); err != nil {
					log.Warn("renew lease", "error", err)
				}
			}
		}
	}()
	return func() { cancel(); <-done }
}

func (r *Runner) execute(ctx context.Context, spec protocol.Job, rep *reporter, result *protocol.JobResult) error {
	switch spec.Kind {
	case protocol.JobDeploy:
		return r.deploy(ctx, spec, rep, result)
	case protocol.JobReset:
		return r.reset(ctx, spec, rep, result)
	case protocol.JobDelete:
		return r.deletePreview(ctx, spec, rep)
	case protocol.JobDeleteMachine:
		return r.deleteMachineJob(ctx, spec, rep)
	default:
		return fmt.Errorf("%w %q", errUnknownJobKind, spec.Kind)
	}
}

// machineKind says which kind of machine name is and, for a runtime
// machine, its preview. ok is false for names this agent never creates.
func (r *Runner) machineKind(name string) (kind, previewID string, ok bool) {
	rest, found := strings.CutPrefix(name, r.MachinePrefix)
	if !found {
		return "", "", false
	}
	if buildWorkerPattern.MatchString(rest) {
		return protocol.MachineBuildWorker, "", true
	}
	if previewIDPattern.MatchString(rest) {
		return protocol.MachineRuntime, rest, true
	}
	return "", "", false
}

func (r *Runner) runtimeName(previewID string) (string, error) {
	if !previewIDPattern.MatchString(previewID) {
		return "", fmt.Errorf("invalid preview id %q", previewID)
	}
	return r.MachinePrefix + previewID, nil
}

func workerName(prefix string, n int) string {
	return prefix + "build-" + strconv.Itoa(n)
}

// link holds the current session, if any.
type link struct {
	mu sync.Mutex
	s  Session
}

var errNoSession = errors.New("no control plane session")

func (l *link) set(s Session) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.s = s
}

func (l *link) clear(s Session) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.s == s {
		l.s = nil
	}
}

func (l *link) send(ctx context.Context, msgType string, payload any) error {
	l.mu.Lock()
	s := l.s
	l.mu.Unlock()
	if s == nil {
		return errNoSession
	}
	return s.Send(ctx, msgType, payload)
}
