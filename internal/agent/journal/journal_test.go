package journal

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func open(t *testing.T, path string) *Journal {
	t.Helper()
	j, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

func TestRecordsSurviveAReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent", "journal.db")
	j := open(t, path)
	if err := j.SetMachine(ctx, "elprovo-s1", "creating"); err != nil {
		t.Fatal(err)
	}
	if err := j.StartJob(ctx, "op1", "lt1", []byte(`{"kind":"deploy"}`)); err != nil {
		t.Fatal(err)
	}
	if err := j.StartJob(ctx, "op2", "lt2", []byte(`{"kind":"delete"}`)); err != nil {
		t.Fatal(err)
	}
	if err := j.FinishJob(ctx, "op2", []byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := open(t, path)
	machines, err := reopened.Machines(ctx)
	if err != nil || !reflect.DeepEqual(machines, map[string]string{"elprovo-s1": "creating"}) {
		t.Fatalf("Machines = %v, %v; want the create intent", machines, err)
	}
	jobs, err := reopened.Jobs(ctx)
	want := []Job{
		{ID: "op1", LeaseToken: "lt1", Spec: []byte(`{"kind":"deploy"}`)},
		{ID: "op2", LeaseToken: "lt2", Spec: []byte(`{"kind":"delete"}`), Result: []byte(`{"ok":true}`)},
	}
	if err != nil || !reflect.DeepEqual(jobs, want) {
		t.Fatalf("Jobs = %+v, %v; want %+v", jobs, err, want)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("journal mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
}

func TestClearedRecordsAreGone(t *testing.T) {
	ctx := context.Background()
	j := open(t, filepath.Join(t.TempDir(), "journal.db"))
	if err := j.SetMachine(ctx, "elprovo-s1", "deleting"); err != nil {
		t.Fatal(err)
	}
	if err := j.SetMachine(ctx, "elprovo-s1", ""); err != nil {
		t.Fatal(err)
	}
	if err := j.StartJob(ctx, "op1", "lt1", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := j.ForgetJob(ctx, "op1"); err != nil {
		t.Fatal(err)
	}

	if m, _ := j.Machines(ctx); len(m) != 0 {
		t.Fatalf("Machines = %v, want none", m)
	}
	if jobs, _ := j.Jobs(ctx); len(jobs) != 0 {
		t.Fatalf("Jobs = %v, want none", jobs)
	}
	if err := j.FinishJob(ctx, "op1", []byte(`{}`)); err == nil {
		t.Fatal("FinishJob of a forgotten job succeeded")
	}
}
