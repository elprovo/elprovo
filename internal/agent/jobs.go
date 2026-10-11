package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/stagely-dev/stagely/internal/agent/build"
	"github.com/stagely-dev/stagely/internal/agent/runtime"
	"github.com/stagely-dev/stagely/protocol"
)

const (
	buildRoot = "/var/lib/stagely/builds/"
	// workerCleanupTimeout bounds cleaning a build worker after its build,
	// which also runs after the job was cancelled.
	workerCleanupTimeout = 2 * time.Minute
)

// deploy builds the commit on a build worker while the stagelet's machine is
// provisioned, then runs the compiled bundle there, runs the migration and
// checks health. The result's bundle names every image by digest, and the
// result says whether the deploy started to apply the bundle.
func (r *Runner) deploy(ctx context.Context, job protocol.Job, rep *reporter, result *protocol.JobResult) error {
	spec := job.Deploy
	if spec == nil {
		return errMissingJobFields
	}
	name, err := r.runtimeName(job.StageletID)
	if err != nil {
		return err
	}
	env, err := r.resolveSecrets(ctx, rep, spec.Secrets)
	if err != nil {
		return err
	}

	provisioned := make(chan error, 1)
	go func() {
		provisioned <- rep.step(ctx, "provision", func(ctx context.Context) error { return r.ensureMachine(ctx, name) })
	}()
	var built build.Result
	buildErr := rep.step(ctx, "build", func(ctx context.Context) error {
		built, err = r.build(ctx, job, rep)
		return err
	})
	if err := errors.Join(buildErr, <-provisioned); err != nil {
		return err
	}

	secretNames := make([]string, 0, len(spec.Secrets))
	for _, s := range spec.Secrets {
		secretNames = append(secretNames, s.Name)
	}
	bundle, err := runtime.Compile(built.Project, built.Images, runtime.BundleSpec{
		PublicService: spec.PublicService, PublicPort: spec.PublicPort, Config: spec.Config, SecretNames: secretNames,
	})
	if err != nil {
		return fmt.Errorf("compile bundle: %w", err)
	}
	m := stepMachine{machines: r.Machines, name: name, rep: rep, step: "start"}
	if err := rep.step(ctx, "start", func(ctx context.Context) error {
		if err := r.registryLogin(ctx, m, false); err != nil {
			return err
		}
		result.MachineChanged = true
		if err := runtime.Apply(ctx, m, bundle, env, false); err != nil {
			return err
		}
		pinned, err := runtime.PinImages(ctx, m, bundle)
		bundle = pinned
		return err
	}); err != nil {
		return err
	}
	if spec.MigrationCommand == "" {
		rep.skip(ctx, "migrate", ".stagely.yml names no migration command")
	} else if err := rep.step(ctx, "migrate", func(ctx context.Context) error {
		m.step = "migrate"
		return runtime.Migrate(ctx, m, spec.PublicService, spec.MigrationCommand)
	}); err != nil {
		return err
	}
	upstream, err := r.checkHealth(ctx, rep, name, spec.PublicPort, spec.HealthPath)
	if err != nil {
		return err
	}
	result.Upstream, result.Images, result.Bundle = upstream, built.Images, bundle
	return nil
}

// build runs the build on a worker from the pool.
func (r *Runner) build(ctx context.Context, job protocol.Job, rep *reporter) (build.Result, error) {
	worker, err := r.pool.acquire(ctx)
	if err != nil {
		return build.Result{}, err
	}
	defer r.pool.release(worker)
	if err := r.ensureMachine(ctx, worker); err != nil {
		return build.Result{}, fmt.Errorf("build worker %s: %w", worker, err)
	}
	m := stepMachine{machines: r.Machines, name: worker, rep: rep, step: "build"}
	defer r.cleanBuildWorker(ctx, m)
	rep.add("build", "agent", "building on "+worker)
	if err := r.registryLogin(ctx, m, true); err != nil {
		return build.Result{}, err
	}
	spec := job.Deploy
	res, err := r.Source.Build(ctx, m, build.Request{
		Source:       build.Commit{APIURL: spec.SourceAPIURL, Repository: spec.Repository, SHA: spec.Commit, Token: spec.SourceToken},
		ComposeFiles: spec.ComposeFiles,
		Config:       spec.Config, Registry: r.Registry.Prefix, Dir: buildRoot + job.StageletID,
	})
	if err != nil {
		return build.Result{}, err
	}
	if _, err := m.Run(ctx, nil, "sudo", "docker", "image", "prune", "--all", "--force", "--filter", "until=72h"); err != nil {
		r.Log.Warn("prune old images on build worker", "worker", worker, "error", err)
	}
	return res, nil
}

// cleanBuildWorker removes what a build leaves on its worker that the next
// build, of any pull request, must not get: the registry login that can
// push, and the BuildKit cache mounts that the build's Dockerfiles could
// write anything to. It keeps the layer cache, whose entries are keyed by
// their inputs. When the cleanup fails, it deletes the worker, and the next
// build creates it again.
func (r *Runner) cleanBuildWorker(ctx context.Context, m stepMachine) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workerCleanupTimeout)
	defer cancel()
	err := r.registryLogout(ctx, m)
	if _, pruneErr := m.Run(ctx, nil, "sudo", "docker", "builder", "prune", "--force", "--filter", "type=exec.cachemount"); pruneErr != nil {
		err = errors.Join(err, fmt.Errorf("delete build cache mounts: %w", pruneErr))
	}
	if err == nil {
		return
	}
	r.Log.Warn("clean build worker failed, deleting it", "worker", m.name, "error", err)
	if err := r.deleteMachine(ctx, m.name); err != nil {
		r.Log.Error("delete build worker after a failed cleanup", "worker", m.name, "error", err)
	}
}

// reset reapplies the bundle of the stagelet's last applied deploy on its
// machine and checks health. It fetches no source and runs no migration. The
// result says whether the reset started to apply the bundle.
func (r *Runner) reset(ctx context.Context, job protocol.Job, rep *reporter, result *protocol.JobResult) error {
	spec := job.Reset
	if spec == nil || len(spec.Bundle) == 0 {
		return errMissingJobFields
	}
	name, err := r.runtimeName(job.StageletID)
	if err != nil {
		return err
	}
	env, err := r.resolveSecrets(ctx, rep, spec.Secrets)
	if err != nil {
		return err
	}
	m := stepMachine{machines: r.Machines, name: name, rep: rep, step: "start"}
	if err := rep.step(ctx, "start", func(ctx context.Context) error {
		exists, err := r.Machines.Exists(ctx, name)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("machine %s is gone; deploy the pull request again", name)
		}
		if err := r.registryLogin(ctx, m, false); err != nil {
			return err
		}
		result.MachineChanged = true
		return runtime.Apply(ctx, m, spec.Bundle, env, true)
	}); err != nil {
		return err
	}
	upstream, err := r.checkHealth(ctx, rep, name, spec.PublicPort, spec.HealthPath)
	if err != nil {
		return err
	}
	result.Upstream = upstream
	return nil
}

// deleteStagelet deletes the stagelet's machine and confirms it is gone.
func (r *Runner) deleteStagelet(ctx context.Context, job protocol.Job, rep *reporter) error {
	name, err := r.runtimeName(job.StageletID)
	if err != nil {
		return err
	}
	return rep.step(ctx, "delete", func(ctx context.Context) error { return r.deleteMachine(ctx, name) })
}

// deleteMachineJob deletes a machine the control plane found no owner for.
// It refuses names this agent never creates.
func (r *Runner) deleteMachineJob(ctx context.Context, job protocol.Job, rep *reporter) error {
	if _, _, ok := r.machineKind(job.Machine); !ok {
		return fmt.Errorf("%w: %q", errMachineNotOwned, job.Machine)
	}
	return rep.step(ctx, "delete", func(ctx context.Context) error { return r.deleteMachine(ctx, job.Machine) })
}

// resolveSecrets returns each referenced secret's value by environment
// variable name, and hides the values from the job's log.
func (r *Runner) resolveSecrets(ctx context.Context, rep *reporter, refs []protocol.SecretRef) (map[string]string, error) {
	env := make(map[string]string, len(refs))
	if len(refs) == 0 {
		return env, nil
	}
	err := rep.step(ctx, "secrets", func(context.Context) error {
		keys := make([]string, 0, len(refs))
		for _, ref := range refs {
			keys = append(keys, ref.Key)
		}
		values, err := r.Secrets.Resolve(keys)
		if err != nil {
			return err
		}
		for _, ref := range refs {
			env[ref.Name] = values[ref.Key]
			rep.hideValues(values[ref.Key])
		}
		return nil
	})
	return env, err
}

func (r *Runner) checkHealth(ctx context.Context, rep *reporter, name string, port int, path string) (string, error) {
	var addr string
	err := rep.step(ctx, "health", func(ctx context.Context) error {
		var err error
		if addr, err = r.Machines.Address(ctx, name); err != nil {
			return err
		}
		m := stepMachine{machines: r.Machines, name: name, rep: rep, step: "health"}
		return runtime.WaitHealthy(ctx, m, port, path, r.HealthTimeout)
	})
	return net.JoinHostPort(addr, strconv.Itoa(port)), err
}

// registryLogin signs the machine's Docker in to the registry, when the
// registry needs credentials, with credentials that can push only when
// push is set.
func (r *Runner) registryLogin(ctx context.Context, m stepMachine, push bool) error {
	if r.Registry.Credentials == nil {
		return nil
	}
	host, _, _ := strings.Cut(r.Registry.Prefix, "/")
	username, password, err := r.Registry.Credentials(ctx, host, push)
	if err != nil {
		return fmt.Errorf("get credentials for registry %s: %w", host, err)
	}
	m.rep.hideValues(username, password)
	if _, err := m.Run(ctx, strings.NewReader(password), "sudo", "docker", "login", "--username", username, "--password-stdin", host); err != nil {
		return fmt.Errorf("sign in to registry %s: %w", host, err)
	}
	return nil
}

// registryLogout removes the login that registryLogin stored on the machine.
func (r *Runner) registryLogout(ctx context.Context, m stepMachine) error {
	if r.Registry.Credentials == nil {
		return nil
	}
	host, _, _ := strings.Cut(r.Registry.Prefix, "/")
	if _, err := m.Run(ctx, nil, "sudo", "docker", "logout", host); err != nil {
		return fmt.Errorf("sign out of registry %s: %w", host, err)
	}
	return nil
}
