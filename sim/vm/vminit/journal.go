package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const unitLogDir = "/var/log/units"

type journal struct {
	dir      string
	hostname string
	mu       sync.Mutex
	logs     map[string]*unitLog
}

func newJournal(dir, hostname string) *journal {
	return &journal{dir: dir, hostname: hostname, logs: map[string]*unitLog{}}
}

func (j *journal) open(unit string) *unitLog {
	j.mu.Lock()
	defer j.mu.Unlock()
	if l, ok := j.logs[unit]; ok {
		return l
	}
	l := &unitLog{hostname: j.hostname}
	if err := os.MkdirAll(j.dir, 0o755); err == nil {
		l.f, _ = os.OpenFile(filepath.Join(j.dir, unit+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	}
	j.logs[unit] = l
	return l
}

type unitLog struct {
	hostname string
	mu       sync.Mutex
	f        *os.File
}

func (l *unitLog) system(msg string) {
	l.line("systemd[1]", msg)
}

func (l *unitLog) line(ident, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		_, _ = fmt.Fprintf(l.f, "%s %s %s: %s\n", time.Now().Format(time.Stamp), l.hostname, ident, msg)
	}
}

func (l *unitLog) copyLines(r io.ReadCloser, ident string, pid int) {
	defer func() { _ = r.Close() }()
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadSlice('\n')
		if len(line) > 0 {
			l.line(ident+"["+strconv.Itoa(pid)+"]", strings.TrimRight(string(line), "\r\n"))
		}
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return
		}
	}
}

func journalctlCommand(root string, args []string, stdout, stderr io.Writer) int {
	opts, err := parseJournalctlArgs(args)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "journalctl:", err)
		return 1
	}
	path := filepath.Join(root, unitLogDir, opts.unit+".log")
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		_, _ = fmt.Fprintln(stderr, "journalctl:", err)
		return 1
	}
	if len(data) == 0 && !opts.follow {
		_, _ = fmt.Fprintln(stdout, "-- No entries --")
		return 0
	}
	_, _ = io.WriteString(stdout, lastLines(string(data), opts.lines))
	if opts.follow {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		follow(ctx, path, int64(len(data)), stdout)
	}
	return 0
}

type journalctlOptions struct {
	unit   string
	lines  int
	follow bool
}

func parseJournalctlArgs(args []string) (journalctlOptions, error) {
	opts := journalctlOptions{lines: -1}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		value := func() (string, error) {
			if _, v, ok := strings.Cut(arg, "="); ok && strings.HasPrefix(arg, "--") {
				return v, nil
			}
			if len(arg) > 2 && !strings.HasPrefix(arg, "--") {
				return arg[2:], nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("option %s needs a value", arg)
			}
			i++
			return args[i], nil
		}
		var err error
		switch {
		case arg == "--no-pager" || arg == "-q" || arg == "--quiet":
		case arg == "-f" || arg == "--follow":
			opts.follow = true
		case strings.HasPrefix(arg, "-u") || strings.HasPrefix(arg, "--unit"):
			if opts.unit != "" {
				return opts, errors.New("only one -u option is supported")
			}
			var name string
			if name, err = value(); err == nil {
				opts.unit, err = normalizeUnitName(name)
			}
		case strings.HasPrefix(arg, "-n") || strings.HasPrefix(arg, "--lines"):
			var n string
			if n, err = value(); err == nil {
				opts.lines, err = strconv.Atoi(n)
			}
		default:
			return opts, fmt.Errorf("option %s is not supported; use -u UNIT, -n N, -f or --no-pager", arg)
		}
		if err != nil {
			return opts, err
		}
	}
	if opts.unit == "" {
		return opts, errors.New("only per-unit logs exist; use -u UNIT")
	}
	return opts, nil
}

func lastLines(text string, n int) string {
	if n < 0 {
		return text
	}
	if text == "" || n == 0 {
		return ""
	}
	lines := strings.SplitAfter(strings.TrimSuffix(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "") + "\n"
}

func follow(ctx context.Context, path string, offset int64, w io.Writer) {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		if _, err := f.Seek(offset, io.SeekStart); err == nil {
			n, _ := io.Copy(w, f)
			offset += n
		}
		_ = f.Close()
	}
}
