// Package journal is the agent's local record of what it has done, in
// SQLite. The agent writes an intent before each side effect and clears or
// completes it after, so a restarted agent knows which machines may be half
// created or half deleted, and which leased jobs it still holds.
package journal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
create table if not exists machines (
    name text primary key,
    state text not null,
    updated_at text not null
);
create table if not exists jobs (
    id text primary key,
    lease_token text not null,
    spec blob not null,
    result blob,
    updated_at text not null
);`

// Journal is an open journal file. It is safe for concurrent use.
type Journal struct {
	db *sql.DB
}

// Job is a leased job the journal holds. Result is nil while the job runs.
type Job struct {
	ID         string
	LeaseToken string
	Spec       []byte
	Result     []byte
}

// Open opens the journal at path, creating it with owner-only permissions
// when it does not exist. Every write is durable when it returns.
func Open(ctx context.Context, path string) (*Journal, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create journal directory: %w", err)
	}
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600); err != nil {
		return nil, fmt.Errorf("create journal: %w", err)
	} else if err := f.Close(); err != nil {
		return nil, fmt.Errorf("create journal: %w", err)
	}
	dsn := (&url.URL{Scheme: "file", Opaque: path, RawQuery: url.Values{"_pragma": {
		"journal_mode(WAL)", "synchronous(FULL)", "busy_timeout(5000)",
	}}.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("prepare journal: %w", err)
	}
	return &Journal{db: db}, nil
}

// Close closes the journal.
func (j *Journal) Close() error {
	return j.db.Close()
}

// SetMachine records that the machine is being created or deleted. An empty
// state clears the record once the side effect is done.
func (j *Journal) SetMachine(ctx context.Context, name, state string) error {
	var err error
	if state == "" {
		_, err = j.db.ExecContext(ctx, `delete from machines where name = ?`, name)
	} else {
		_, err = j.db.ExecContext(ctx, `insert into machines (name, state, updated_at) values (?, ?, ?)
			on conflict (name) do update set state = excluded.state, updated_at = excluded.updated_at`, name, state, now())
	}
	if err != nil {
		return fmt.Errorf("record machine %s: %w", name, err)
	}
	return nil
}

// Machines returns every machine with an unfinished create or delete, by
// name.
func (j *Journal) Machines(ctx context.Context) (map[string]string, error) {
	rows, err := j.db.QueryContext(ctx, `select name, state from machines`)
	if err != nil {
		return nil, fmt.Errorf("read machines: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var name, state string
		if err := rows.Scan(&name, &state); err != nil {
			return nil, fmt.Errorf("read machines: %w", err)
		}
		out[name] = state
	}
	return out, rows.Err()
}

// StartJob records a job the agent took, before it does any work for it.
func (j *Journal) StartJob(ctx context.Context, id, leaseToken string, spec []byte) error {
	_, err := j.db.ExecContext(ctx, `insert into jobs (id, lease_token, spec, updated_at) values (?, ?, ?, ?)
		on conflict (id) do update set lease_token = excluded.lease_token, spec = excluded.spec, result = null, updated_at = excluded.updated_at`,
		id, leaseToken, spec, now())
	if err != nil {
		return fmt.Errorf("record job %s: %w", id, err)
	}
	return nil
}

// FinishJob records a job's result until the control plane has it.
func (j *Journal) FinishJob(ctx context.Context, id string, result []byte) error {
	res, err := j.db.ExecContext(ctx, `update jobs set result = ?, updated_at = ? where id = ?`, result, now(), id)
	if err != nil {
		return fmt.Errorf("record result of job %s: %w", id, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("record result of job %s: %w", id, errUnknownJob)
	}
	return nil
}

// ForgetJob removes a job the control plane has a result for, or no longer
// wants.
func (j *Journal) ForgetJob(ctx context.Context, id string) error {
	if _, err := j.db.ExecContext(ctx, `delete from jobs where id = ?`, id); err != nil {
		return fmt.Errorf("forget job %s: %w", id, err)
	}
	return nil
}

// Jobs returns the jobs the journal holds, oldest first.
func (j *Journal) Jobs(ctx context.Context) ([]Job, error) {
	rows, err := j.db.QueryContext(ctx, `select id, lease_token, spec, result from jobs order by updated_at, id`)
	if err != nil {
		return nil, fmt.Errorf("read jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Job
	for rows.Next() {
		var job Job
		if err := rows.Scan(&job.ID, &job.LeaseToken, &job.Spec, &job.Result); err != nil {
			return nil, fmt.Errorf("read jobs: %w", err)
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

var errUnknownJob = errors.New("the journal holds no such job")

func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
