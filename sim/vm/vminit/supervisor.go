package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultPath    = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	stopTimeout    = 10 * time.Second
	startTimeout   = 90 * time.Second
	maxRestartWait = 30 * time.Second
	stableRun      = 10 * time.Second
)

type supervisor struct {
	root      string
	runDir    string
	journal   *journal
	mu        sync.Mutex
	services  map[string]*service
	closed    bool
	launching sync.WaitGroup
}

type service struct {
	def     unit
	running bool
	state   string
	sub     string
	pid     int
	since   time.Time
	stop    chan struct{}
	done    chan struct{}
}

func newSupervisor(root, runDir string, j *journal) *supervisor {
	return &supervisor{root: root, runDir: runDir, journal: j, services: map[string]*service{}}
}

func (s *supervisor) start(name string) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("the machine is shutting down")
	}
	svc, err := s.loadLocked(name)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if svc.running {
		s.mu.Unlock()
		return nil
	}
	svc.running = true
	svc.stop = make(chan struct{})
	svc.done = make(chan struct{})
	svc.setState("activating", "start")
	started := make(chan error, 1)
	def, stop, done := svc.def, svc.stop, svc.done
	s.launching.Go(func() { s.run(svc, def, stop, done, started) })
	s.mu.Unlock()
	return <-started
}

func (s *supervisor) stop(name string) error {
	s.mu.Lock()
	svc, ok := s.services[name]
	if !ok || !svc.running {
		s.mu.Unlock()
		if !ok && !s.exists(name) {
			return missingUnitError{name, "loaded"}
		}
		return nil
	}
	stop, done := svc.stop, svc.done
	if !isClosed(stop) {
		close(stop)
	}
	s.mu.Unlock()
	<-done
	return nil
}

func (s *supervisor) restart(name string) error {
	_ = s.stop(name)
	return s.start(name)
}

func (s *supervisor) stopAll() {
	s.mu.Lock()
	s.closed = true
	var names []string
	for name, svc := range s.services {
		if svc.running {
			names = append(names, name)
		}
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, name := range names {
		wg.Go(func() { _ = s.stop(name) })
	}
	wg.Wait()
	s.launching.Wait()
}

func (s *supervisor) reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	for name, svc := range s.services {
		def, err := loadUnit(s.root, name)
		switch {
		case errors.Is(err, os.ErrNotExist) && !svc.running:
			delete(s.services, name)
		case err != nil:
			errs = append(errs, err)
		default:
			svc.def = def
		}
	}
	return errors.Join(errs...)
}

func (s *supervisor) exists(name string) bool {
	_, err := os.Stat(filepath.Join(s.root, unitDir, name))
	return err == nil
}

func (s *supervisor) loadLocked(name string) (*service, error) {
	if svc, ok := s.services[name]; ok {
		return svc, nil
	}
	def, err := loadUnit(s.root, name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, missingUnitError{name, "found"}
	}
	if err != nil {
		return nil, err
	}
	svc := &service{def: def, state: "inactive", sub: "dead"}
	s.services[name] = svc
	return svc, nil
}

func (s *supervisor) snapshot(name string) (service, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	svc, ok := s.services[name]
	if !ok {
		return service{}, false
	}
	return *svc, true
}

func (s *supervisor) update(svc *service, fn func(*service)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(svc)
}

func (svc *service) setState(state, sub string) {
	if svc.state != state {
		svc.since = time.Now()
	}
	svc.state, svc.sub = state, sub
}

func (s *supervisor) run(svc *service, def unit, stop, done chan struct{}, started chan<- error) {
	log := s.journal.open(def.name)
	defer func() {
		s.update(svc, func(svc *service) { svc.running, svc.pid = false, 0 })
		close(done)
	}()
	for _, w := range def.warnings {
		log.system(w)
	}
	report := &startReport{ch: started}
	delay := def.restartDelay
	for restarts := 1; ; restarts++ {
		began := time.Now()
		failed := s.attempt(svc, def, log, stop, report) != nil
		if isClosed(stop) {
			s.stopped(svc, def, log)
			return
		}
		if def.restart != "always" && (def.restart != "on-failure" || !failed) {
			s.ended(svc, def, log, failed)
			return
		}
		if time.Since(began) >= stableRun {
			delay = def.restartDelay
		}
		s.update(svc, func(svc *service) { svc.setState("activating", "auto-restart") })
		log.system(fmt.Sprintf("%s: Scheduled restart job, restart counter is at %d.", def.name, restarts))
		select {
		case <-stop:
			s.stopped(svc, def, log)
			return
		case <-time.After(delay):
		}
		delay = min(2*delay+time.Second/10, maxRestartWait)
	}
}

func (s *supervisor) stopped(svc *service, def unit, log *unitLog) {
	s.update(svc, func(svc *service) { svc.setState("inactive", "dead") })
	log.system("Stopped " + describe(def) + ".")
}

func (s *supervisor) ended(svc *service, def unit, log *unitLog, failed bool) {
	if !failed {
		s.update(svc, func(svc *service) { svc.setState("inactive", "dead") })
		return
	}
	s.update(svc, func(svc *service) { svc.setState("failed", "failed") })
	log.system(def.name + ": Failed with result 'exit-code'.")
}

func (s *supervisor) attempt(svc *service, def unit, log *unitLog, stop <-chan struct{}, report *startReport) error {
	err := s.prepareStateDirectories(def)
	var env []string
	if err == nil {
		env, err = s.environment(def)
	}
	if err == nil {
		err = runAll(def, def.execStartPre, env, log, stop)
	}
	if err != nil {
		return report.fail(def, log, err)
	}
	if def.serviceType == "oneshot" {
		return s.oneshot(svc, def, env, log, stop, report)
	}
	return s.daemon(svc, def, env, log, stop, report)
}

func (s *supervisor) daemon(svc *service, def unit, env []string, log *unitLog, stop <-chan struct{}, report *startReport) error {
	var notify *notifySocket
	if def.serviceType == "notify" {
		var err error
		if notify, err = listenNotify(s.runDir, def.name); err != nil {
			return report.fail(def, log, err)
		}
		defer notify.Close()
		env = append(env, "NOTIFY_SOCKET="+notify.path)
	}
	proc, err := spawn(def, def.execStart[0], env, log)
	if err != nil {
		return report.fail(def, log, err)
	}
	s.update(svc, func(svc *service) { svc.pid = proc.pid })
	if notify != nil {
		select {
		case <-notify.ready:
		case err := <-proc.exited:
			return report.fail(def, log, exitError(err))
		case <-stop:
			proc.terminate()
			report.send(errors.New("the unit was stopped while it started"))
			return nil
		case <-time.After(startTimeout):
			proc.terminate()
			return report.fail(def, log, errors.New("start operation timed out"))
		}
	}
	s.update(svc, func(svc *service) { svc.setState("active", "running") })
	log.system("Started " + describe(def) + ".")
	report.send(nil)
	select {
	case err := <-proc.exited:
		if err != nil {
			err = exitError(err)
			log.system(fmt.Sprintf("%s: Main process exited, %v", def.name, err))
		}
		return err
	case <-stop:
		s.update(svc, func(svc *service) { svc.setState("deactivating", "stop") })
		log.system("Stopping " + describe(def) + "...")
		proc.terminate()
		return nil
	}
}

func (s *supervisor) oneshot(svc *service, def unit, env []string, log *unitLog, stop <-chan struct{}, report *startReport) error {
	if err := runAll(def, def.execStart, env, log, stop); err != nil {
		return report.fail(def, log, err)
	}
	log.system("Finished " + describe(def) + ".")
	if def.remainAfterExit {
		s.update(svc, func(svc *service) { svc.setState("active", "exited") })
	}
	report.send(nil)
	if def.remainAfterExit {
		<-stop
	}
	return nil
}

func runAll(def unit, lines []execLine, env []string, log *unitLog, stop <-chan struct{}) error {
	for _, line := range lines {
		proc, err := spawn(def, line, env, log)
		if err == nil {
			select {
			case err = <-proc.exited:
				err = exitError(err)
			case <-stop:
				proc.terminate()
				return errors.New("the unit was stopped")
			}
		}
		if err != nil && !line.ignoreFailure {
			return err
		}
	}
	return nil
}

type startReport struct{ ch chan<- error }

func (r *startReport) send(err error) {
	if r.ch != nil {
		r.ch <- err
		r.ch = nil
	}
}

func (r *startReport) fail(def unit, log *unitLog, err error) error {
	log.system(def.name + ": " + err.Error())
	r.send(startFailedError{def.name})
	return err
}

func (s *supervisor) environment(def unit) ([]string, error) {
	env := []string{"PATH=" + defaultPath}
	if def.user != "" {
		u, err := user.Lookup(def.user)
		if err != nil {
			return nil, err
		}
		env = append(env, "HOME="+u.HomeDir, "USER="+u.Username, "LOGNAME="+u.Username, "SHELL=/bin/sh")
	}
	if len(def.stateDirectories) > 0 {
		paths := make([]string, len(def.stateDirectories))
		for i, dir := range def.stateDirectories {
			paths[i] = "/var/lib/" + dir
		}
		env = append(env, "STATE_DIRECTORY="+strings.Join(paths, ":"))
	}
	env = append(env, def.environment...)
	for _, spec := range def.environmentFiles {
		vars, err := readEnvironmentFile(spec)
		if err != nil {
			return nil, fmt.Errorf("failed to load environment file %s: %w", spec, err)
		}
		env = append(env, vars...)
	}
	return env, nil
}

// prepareStateDirectories creates the unit's state directories under
// /var/lib with its mode, owned by the unit's user and group, as systemd does
// before each start.
func (s *supervisor) prepareStateDirectories(def unit) error {
	cred, err := credential(def)
	if err != nil {
		return err
	}
	for _, dir := range def.stateDirectories {
		path := filepath.Join(s.root, "var/lib", dir)
		if err := os.MkdirAll(path, def.stateDirectoryMode); err != nil {
			return fmt.Errorf("create state directory %s: %w", dir, err)
		}
		if err := os.Chmod(path, def.stateDirectoryMode); err != nil {
			return fmt.Errorf("set the mode of state directory %s: %w", dir, err)
		}
		if cred != nil {
			if err := os.Chown(path, int(cred.Uid), int(cred.Gid)); err != nil {
				return fmt.Errorf("give state directory %s to its user: %w", dir, err)
			}
		}
	}
	return nil
}

type process struct {
	pid    int
	exited chan error
}

func spawn(def unit, line execLine, env []string, log *unitLog) (*process, error) {
	argv, err := line.argv(env)
	if err != nil {
		return nil, err
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return nil, err
	}
	cred, err := credential(def)
	if err != nil {
		return nil, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(path, argv[1:]...) //nolint:noctx // unit processes belong to no request; terminate stops their process group
	cmd.Env, cmd.Dir, cmd.Stdout, cmd.Stderr = env, def.workingDirectory, w, w
	cmd.SysProcAttr = processAttributes(cred, def.ambientCaps)
	err = cmd.Start()
	_ = w.Close()
	if err != nil {
		_ = r.Close()
		return nil, err
	}
	ident := filepath.Base(argv[0])
	pid := cmd.Process.Pid
	go log.copyLines(r, ident, pid)
	p := &process{pid: pid, exited: make(chan error, 1)}
	go func() { p.exited <- cmd.Wait() }()
	return p, nil
}

func (p *process) terminate() {
	_ = syscall.Kill(-p.pid, syscall.SIGTERM)
	select {
	case <-p.exited:
		return
	case <-time.After(stopTimeout):
	}
	_ = syscall.Kill(-p.pid, syscall.SIGKILL)
	<-p.exited
}

func credential(def unit) (*syscall.Credential, error) {
	if def.user == "" && def.group == "" {
		return nil, nil
	}
	cred := &syscall.Credential{}
	if def.user != "" {
		u, err := user.Lookup(def.user)
		if err != nil {
			return nil, err
		}
		uid, _ := strconv.ParseUint(u.Uid, 10, 32)
		gid, _ := strconv.ParseUint(u.Gid, 10, 32)
		cred.Uid, cred.Gid = uint32(uid), uint32(gid)
		groups, _ := u.GroupIds()
		for _, g := range groups {
			if id, err := strconv.ParseUint(g, 10, 32); err == nil {
				cred.Groups = append(cred.Groups, uint32(id))
			}
		}
	}
	if def.group != "" {
		g, err := user.LookupGroup(def.group)
		if err != nil {
			return nil, err
		}
		gid, _ := strconv.ParseUint(g.Gid, 10, 32)
		cred.Gid = uint32(gid)
	}
	return cred, nil
}

func exitError(err error) error {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return err
	}
	if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return fmt.Errorf("code=killed, status=%d/%s", int(status.Signal()), strings.ToUpper(status.Signal().String()))
	}
	return fmt.Errorf("code=exited, status=%d/FAILURE", exit.ExitCode())
}

func describe(def unit) string {
	if def.description == "" {
		return def.name
	}
	return def.name + " - " + def.description
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

type notifySocket struct {
	path  string
	conn  *net.UnixConn
	ready chan struct{}
}

func listenNotify(runDir, name string) (*notifySocket, error) {
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(runDir, name+".notify")
	_ = os.Remove(path)
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o666); err != nil {
		_ = conn.Close()
		return nil, err
	}
	n := &notifySocket{path: path, conn: conn, ready: make(chan struct{})}
	go n.read()
	return n, nil
}

func (n *notifySocket) read() {
	buf := make([]byte, 4096)
	for {
		k, err := n.conn.Read(buf)
		if err != nil {
			return
		}
		for _, field := range strings.Split(string(buf[:k]), "\n") {
			if field == "READY=1" && !isClosed(n.ready) {
				close(n.ready)
			}
		}
	}
}

func (n *notifySocket) Close() {
	_ = n.conn.Close()
	_ = os.Remove(n.path)
}
