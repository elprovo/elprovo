package lima

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeLimactl is a limactl that keeps its VMs in a file. It records each
// call, and the standard input of each command run in a VM. With a stuck
// file present, delete leaves the VM listed.
const fakeLimactl = `#!/bin/sh
dir=$(dirname "$0")
printf '%s\n' "$*" >> "$dir/calls"
case "$1" in
create)
	for a in "$@"; do case "$a" in --name=*) echo "${a#--name=}" >> "$dir/vms" ;; esac; done ;;
list)
	[ -f "$dir/vms" ] && while read -r n; do printf '{"name":"%s"}\n' "$n"; done < "$dir/vms" ;;
delete)
	for n; do :; done
	[ -f "$dir/stuck" ] || { grep -vx "$n" "$dir/vms" > "$dir/vms.new"; mv "$dir/vms.new" "$dir/vms"; } ;;
shell)
	cat >> "$dir/stdin" ;;
esac
`

func newFakeProvider(t *testing.T) (*Provider, string) {
	t.Helper()
	dir := t.TempDir()
	limactl := filepath.Join(dir, "limactl")
	if err := os.WriteFile(limactl, []byte(fakeLimactl), 0o755); err != nil {
		t.Fatal(err)
	}
	p := New()
	p.Limactl = limactl
	return p, dir
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestCreateStartsAVMWithDockerThatIsolatesContainersAndTrustsTheRegistry(t *testing.T) {
	p, dir := newFakeProvider(t)
	p.InsecureRegistries = []string{"host.lima.internal:5052"}

	if err := p.Create(context.Background(), "elprovo-local-0123456789ab"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	calls := readLines(t, filepath.Join(dir, "calls"))
	want := []string{
		"create --tty=false --name=elprovo-local-0123456789ab --vm-type=vz --network=lima:user-v2 --plain --cpus=2 --memory=4 --disk=20 template:docker-rootful",
		"start --tty=false elprovo-local-0123456789ab",
	}
	if len(calls) < 2 || !slices.Equal(calls[:2], want) {
		t.Fatalf("first calls = %q, want %q", calls, want)
	}
	if last := calls[len(calls)-1]; !strings.HasPrefix(last, "shell --workdir / elprovo-local-0123456789ab -- sudo sh -c systemctl daemon-reload && systemctl restart docker") {
		t.Fatalf("last call = %q, want Docker restarted with the new settings", last)
	}
	written := strings.Join(readLines(t, filepath.Join(dir, "stdin")), "\n")
	for _, part := range []string{
		`iptables -A DOCKER-USER -d "$net" -m conntrack --ctstate NEW -j REJECT`,
		"ExecStartPost=/usr/local/sbin/elprovo-container-isolation",
		`{"insecure-registries":["host.lima.internal:5052"]}`,
	} {
		if !strings.Contains(written, part) {
			t.Errorf("the files written in the VM lack %q", part)
		}
	}
	if exists, err := p.Exists(context.Background(), "elprovo-local-0123456789ab"); err != nil || !exists {
		t.Fatalf("Exists after Create = %v, %v; want true", exists, err)
	}
}

func TestDeleteRemovesTheVMAndConfirmsItIsGone(t *testing.T) {
	ctx := context.Background()
	p, dir := newFakeProvider(t)
	if err := p.Create(ctx, "elprovo-local-0123456789ab"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := p.Delete(ctx, "elprovo-local-0123456789ab"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if exists, err := p.Exists(ctx, "elprovo-local-0123456789ab"); err != nil || exists {
		t.Fatalf("Exists after Delete = %v, %v; want false", exists, err)
	}
	if err := p.Delete(ctx, "elprovo-local-0123456789ab"); err != nil {
		t.Fatalf("Delete of a missing VM: %v, want success", err)
	}
	if deletes := slices.DeleteFunc(readLines(t, filepath.Join(dir, "calls")), func(c string) bool { return !strings.HasPrefix(c, "delete ") }); len(deletes) != 1 {
		t.Fatalf("delete calls = %q, want one: a missing VM needs no delete", deletes)
	}
}

func TestDeleteFailsWhenTheVMIsStillListed(t *testing.T) {
	ctx := context.Background()
	p, dir := newFakeProvider(t)
	if err := p.Create(ctx, "elprovo-local-0123456789ab"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stuck"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	err := p.Delete(ctx, "elprovo-local-0123456789ab")

	if err == nil || !strings.Contains(err.Error(), "still listed") {
		t.Fatalf("Delete = %v, want an error that the VM is still listed", err)
	}
}
