package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/elprovo/elprovo-next/internal/replay"
	"github.com/elprovo/elprovo-next/internal/testrepo"
)

// goConfig is an elprovo.toml that runs go test, with the caches of the go
// command that runs these tests.
const goConfig = "version = 2\npass_env = [\"GOCACHE\", \"GOMODCACHE\"]\n\n" +
	"[tests]\nfiles = [\"**/*_test.go\"]\nrun = \"go test -json -count=1 {packages}\"\nreport = \"go-test-json\"\n"

// The cart package of a shop. buggyCart takes a discount twice, and
// fixedCart once.
const (
	buggyCart  = "package cart\n\n// Total returns the price after a discount.\nfunc Total(price, discount int) int { return price - 2*discount }\n"
	fixedCart  = "package cart\n\n// Total returns the price after a discount.\nfunc Total(price, discount int) int { return price - discount }\n"
	couponTest = "package cart\n\nimport \"testing\"\n\nfunc TestCouponAppliesOnce(t *testing.T) {\n" +
		"\tif got := Total(100, 10); got != 90 {\n\t\tt.Errorf(\"Total(100, 10) = %d; want 90\", got)\n\t}\n}\n"
	noDiscountTest = "package cart\n\nimport \"testing\"\n\nfunc TestNoDiscount(t *testing.T) {\n" +
		"\tif got := Total(100, 0); got != 100 {\n\t\tt.Errorf(\"Total(100, 0) = %d; want 100\", got)\n\t}\n}\n"
)

// shop makes a repository whose base commit has the cart package with the
// code cart and the test TestNoDiscount, and returns it with the base commit.
func shop(t *testing.T, cart string) (testrepo.Repo, string) {
	t.Helper()
	testrepo.ShareGoCaches(t)
	repo := testrepo.New(t)
	base := repo.Commit(testrepo.GoModule(map[string]string{"elprovo.toml": goConfig, "cart/cart.go": cart, "cart/total_test.go": noDiscountTest}))
	return repo, base
}

// scriptConfig is an elprovo.toml whose test command is run.sh. top holds
// fields before the tables, tests more fields of [tests], and app the
// fields of [app], when it is not empty.
func scriptConfig(top, tests, app string) string {
	config := "version = 2\n" + top + "\n[tests]\nfiles = [\"**/*_test.go\"]\nrun = \"sh run.sh {packages}\"\nreport = \"go-test-json\"\n" + tests
	if app != "" {
		config += "\n[app]\n" + app
	}
	return config
}

// event is a shell line that prints the go test -json event that ends the
// test name in the root package of testrepo.Module.
func event(action, name string) string {
	return fmt.Sprintf(`printf '%%s\n' '{"Action":"%s","Package":"%s","Test":"%s"}'`+"\n", action, testrepo.Module, name)
}

// scriptRepo makes a repository whose base commit has config and run.sh,
// and whose head commit changes a Go file. It returns the repository and
// the base commit.
func scriptRepo(t *testing.T, config, script string) (testrepo.Repo, string) {
	t.Helper()
	repo := testrepo.New(t)
	base := repo.Commit(testrepo.GoModule(map[string]string{"elprovo.toml": config, "run.sh": script, "a.go": "package m\n"}))
	repo.Commit(map[string]string{"a.go": "package m\n\nconst A = 1\n"})
	return repo, base
}

type result struct {
	status         int
	stdout, stderr string
	report         replay.Report
}

func run(t *testing.T, ctx context.Context, dir string, args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	status := Run(ctx, "test", args, Env{Stdout: &stdout, Stderr: &stderr, Environ: os.Environ(), Dir: dir})
	r := result{status: status, stdout: stdout.String(), stderr: stderr.String()}
	reports, _ := filepath.Glob(filepath.Join(dir, ".elprovo", "runs", "*", "report.json"))
	if len(reports) > 1 {
		t.Fatalf("found %d reports; each test replays once", len(reports))
	}
	if len(reports) == 1 {
		data, err := os.ReadFile(reports[0])
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &r.report); err != nil {
			t.Fatalf("report.json is not valid: %v\n%s", err, data)
		}
	}
	return r
}

func replayIn(t *testing.T, dir string, args ...string) result {
	t.Helper()
	return run(t, context.Background(), dir, append([]string{"replay"}, args...)...)
}

func (r result) test(t *testing.T, name string) replay.TestReport {
	t.Helper()
	for _, test := range append(r.report.Changed, r.report.Regressions...) {
		if test.Name == name {
			return test
		}
	}
	t.Fatalf("the report has no test %s\nstdout:\n%s\nstderr:\n%s", name, r.stdout, r.stderr)
	return replay.TestReport{}
}

func outcomesOf(runs []replay.TestRun) string {
	var list []string
	for _, run := range runs {
		list = append(list, string(run.Outcome))
	}
	return strings.Join(list, " ")
}

func wantVerdict(t *testing.T, r result, name string, want replay.Verdict, wantBase, wantHead string, wantStatus int) {
	t.Helper()
	test := r.test(t, name)
	if test.Verdict != want || outcomesOf(test.Base) != wantBase || outcomesOf(test.Head) != wantHead || r.status != wantStatus {
		t.Errorf("%s: verdict %q, base %q, head %q, exit %d; want %q, base %q, head %q, exit %d\nstdout:\n%s\nstderr:\n%s",
			name, test.Verdict, outcomesOf(test.Base), outcomesOf(test.Head), r.status,
			want, wantBase, wantHead, wantStatus, r.stdout, r.stderr)
	}
}

func wantLines(t *testing.T, r result, lines ...string) {
	t.Helper()
	for _, line := range lines {
		if !strings.Contains(r.stdout, line+"\n") {
			t.Errorf("stdout has no line %q:\n%s", line, r.stdout)
		}
	}
}

func (r result) replayID(t *testing.T) string {
	t.Helper()
	const prefix = "Report: .elprovo/runs/"
	for _, line := range strings.Split(r.stdout, "\n") {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			return strings.TrimSuffix(rest, "/report.json")
		}
	}
	t.Fatalf("stdout has no report line:\n%s", r.stdout)
	return ""
}

func TestAFixReplaysAsFixedAndTheOtherTestsAsKept(t *testing.T) {
	repo, base := shop(t, buggyCart)
	repo.Commit(map[string]string{"cart/cart.go": fixedCart, "cart/cart_test.go": couponTest})

	r := replayIn(t, repo.Dir, "--base", base)

	wantVerdict(t, r, "TestCouponAppliesOnce", replay.Fixed, "failed failed", "passed passed", 0)
	wantVerdict(t, r, "TestNoDiscount", replay.Kept, "passed passed", "passed passed", 0)
	wantLines(t, r,
		"packages: ./cart",
		"✓ fixed           TestCouponAppliesOnce  cart/cart_test.go",
		indent+"base: failed 2 of 2   head: passed 2 of 2",
		indent+"base run 1: cart_test.go:7: Total(100, 10) = 80; want 90",
		"Changed tests: 1 fixed.",
		"Other tests: 1 kept.",
	)
	if strings.Contains(r.stdout, "TestNoDiscount") {
		t.Errorf("stdout lists a kept test:\n%s", r.stdout)
	}
	if got := r.test(t, "TestCouponAppliesOnce").Base[0].Message; got != "cart_test.go:7: Total(100, 10) = 80; want 90" {
		t.Errorf("the message of base run 1 is %q; want the failure line", got)
	}
}

func TestATestThatPassesWithoutTheChangeIsNotReproduced(t *testing.T) {
	repo, base := shop(t, fixedCart)
	repo.Commit(map[string]string{"cart/cart.go": fixedCart + "\n// Totals are in cents.\n", "cart/cart_test.go": couponTest})

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantVerdict(t, r, "TestCouponAppliesOnce", replay.NotReproduced, "passed", "passed", 1)
	wantLines(t, r, "✗ not reproduced  TestCouponAppliesOnce  cart/cart_test.go")
}

func TestATestOfNewCodeIsNewAndItsPackageIsNotRunOnTheBase(t *testing.T) {
	repo, base := shop(t, fixedCart)
	repo.Commit(map[string]string{
		"cart/cart.go": fixedCart + "\n// Free reports whether an order ships for free.\nfunc Free(total int) bool { return total >= 50 }\n",
		"cart/free_test.go": "package cart\n\nimport \"testing\"\n\nfunc TestFreeShipping(t *testing.T) {\n" +
			"\tif !Free(50) {\n\t\tt.Error(\"an order of 50 ships for a fee\")\n\t}\n}\n",
	})

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantVerdict(t, r, "TestFreeShipping", replay.New, "absent", "passed", 0)
	wantVerdict(t, r, "TestNoDiscount", replay.NotRun, "absent", "passed", 0)
	wantLines(t, r,
		"! new             TestFreeShipping  cart/free_test.go",
		indent+"base run 1: cart/free_test.go:6:6: undefined: Free",
		"! not run         TestNoDiscount  ./cart",
	)
}

func TestATestThatFailsWithTheChangeIsNotFixed(t *testing.T) {
	repo, base := shop(t, buggyCart)
	repo.Commit(map[string]string{"cart/cart_test.go": couponTest})

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantVerdict(t, r, "TestCouponAppliesOnce", replay.NotFixed, "failed", "failed", 1)
	wantLines(t, r,
		"✗ not fixed       TestCouponAppliesOnce  cart/cart_test.go",
		indent+"head run 1: cart_test.go:7: Total(100, 10) = 80; want 90",
		indent+"log: "+filepath.Join(".elprovo", "runs", r.replayID(t), "head-1", "test.log"),
	)
}

func TestAnUnchangedTestThatFailsOnBothCommitsIsAlreadyFailing(t *testing.T) {
	repo, _ := shop(t, buggyCart)
	base := repo.Commit(map[string]string{"cart/coupon_test.go": couponTest})
	repo.Commit(map[string]string{"cart/cart.go": buggyCart + "\n// Totals are in cents.\n"})

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantVerdict(t, r, "TestCouponAppliesOnce", replay.AlreadyFailing, "failed", "failed", 0)
	wantVerdict(t, r, "TestNoDiscount", replay.Kept, "passed", "passed", 0)
	wantLines(t, r,
		"! already failing TestCouponAppliesOnce  ./cart",
		indent+"head run 1: coupon_test.go:7: Total(100, 10) = 80; want 90",
		"Changed tests: none.",
		"Other tests: 1 kept, 1 already failing.",
	)
}

func TestAChangeThatBreaksAnotherTestFails(t *testing.T) {
	repo, base := shop(t, fixedCart)
	repo.Commit(map[string]string{"cart/cart.go": "package cart\n\n// Total returns the price after a discount.\nfunc Total(price, discount int) int { return price - discount + 1 }\n"})

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantVerdict(t, r, "TestNoDiscount", replay.Broke, "passed", "failed", 1)
	wantLines(t, r,
		"✗ broke           TestNoDiscount  ./cart",
		indent+"head run 1: total_test.go:7: Total(100, 0) = 101; want 100",
		indent+"log: "+filepath.Join(".elprovo", "runs", r.replayID(t), "head-1", "test.log"),
		"Other tests: 1 broke.",
	)
}

func TestAChangeThatStopsAnotherTestFromBuildingBrokeIt(t *testing.T) {
	repo, base := shop(t, fixedCart)
	repo.Commit(map[string]string{"cart/cart.go": strings.Replace(fixedCart, "func Total(", "func Sum(", 1)})

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantVerdict(t, r, "TestNoDiscount", replay.Broke, "passed", "absent", 1)
	wantLines(t, r,
		"✗ broke           TestNoDiscount  ./cart",
		indent+"head run 1: cart/total_test.go:6:12: undefined: Total",
	)
}

func TestAPanicOnTheBaseLeavesTheLaterTestsNotRun(t *testing.T) {
	testrepo.ShareGoCaches(t)
	repo := testrepo.New(t)
	panicking := "package cart\n\n// Total returns the price after a discount.\nfunc Total(price, discount int) int {\n\tvar seen map[int]bool\n\tseen[discount] = true\n\treturn price - discount\n}\n"
	tests := "package cart\n\nimport \"testing\"\n\nfunc TestTotal(t *testing.T) {\n\tif Total(100, 10) != 90 {\n\t\tt.Error(\"wrong total\")\n\t}\n}\n\n" +
		"func TestLater(t *testing.T) {}\n"
	base := repo.Commit(testrepo.GoModule(map[string]string{"elprovo.toml": goConfig, "cart/cart.go": panicking, "cart/cart_test.go": tests}))
	repo.Commit(map[string]string{"cart/cart.go": fixedCart})

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantVerdict(t, r, "TestTotal", replay.Fixed, "failed", "passed", 0)
	wantVerdict(t, r, "TestLater", replay.NotRun, "absent", "passed", 0)
	wantLines(t, r, indent+"base run 1: panic: assignment to entry in nil map [recovered, repanicked]")
}

func TestAChangedTestThatDisagreesWithItselfIsFlaky(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "counter")
	if err := os.WriteFile(counter, []byte("0"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COUNTER", counter)
	repo, _ := shop(t, fixedCart)
	// Base runs use the base commit's elprovo.toml, so it passes COUNTER too.
	base := repo.Commit(map[string]string{"elprovo.toml": strings.Replace(goConfig, `"GOMODCACHE"]`, `"GOMODCACHE", "COUNTER"]`, 1)})
	repo.Commit(map[string]string{
		// The runs go base 1, head 1, base 2, head 2. The test fails in the
		// first and the last of them.
		"cart/counted_test.go": "package cart\n\nimport (\n\t\"os\"\n\t\"strconv\"\n\t\"testing\"\n)\n\n" +
			"func TestCounted(t *testing.T) {\n" +
			"\tdata, _ := os.ReadFile(os.Getenv(\"COUNTER\"))\n" +
			"\tn, _ := strconv.Atoi(string(data))\n" +
			"\tos.WriteFile(os.Getenv(\"COUNTER\"), []byte(strconv.Itoa(n+1)), 0o644)\n" +
			"\tif n%3 == 0 {\n\t\tt.Fatalf(\"count %d\", n)\n\t}\n}\n",
	})

	r := replayIn(t, repo.Dir, "--base", base)

	wantVerdict(t, r, "TestCounted", replay.Flaky, "failed passed", "passed failed", 1)
	wantLines(t, r,
		"✗ flaky           TestCounted  cart/counted_test.go",
		indent+"base: passed 1, failed 1 of 2   head: passed 1, failed 1 of 2",
		indent+"base run 1: counted_test.go:14: count 0",
		indent+"log: "+filepath.Join(".elprovo", "runs", r.replayID(t), "base-1", "test.log"),
	)
}

func TestASkippedTestIsSkipped(t *testing.T) {
	repo, base := shop(t, buggyCart)
	repo.Commit(map[string]string{
		"cart/cart.go":         fixedCart,
		"cart/network_test.go": "package cart\n\nimport \"testing\"\n\nfunc TestNetwork(t *testing.T) {\n\tt.Skip(\"needs a network\")\n}\n",
	})

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantVerdict(t, r, "TestNetwork", replay.Skipped, "skipped", "skipped", 0)
	wantLines(t, r, "! skipped         TestNetwork  cart/network_test.go", indent+"base run 1: network_test.go:6: needs a network")
}

func reproduceIn(t *testing.T, dir string, args ...string) result {
	t.Helper()
	return run(t, context.Background(), dir, append([]string{"reproduce"}, args...)...)
}

func TestReproduceShowsThatATestFailsWithoutTheFix(t *testing.T) {
	repo, base := shop(t, buggyCart)
	repo.Commit(map[string]string{"cart/cart_test.go": couponTest})
	cart := filepath.Join(repo.Dir, "cart")

	r := reproduceIn(t, cart, "--base", base, "cart_test.go")

	wantVerdict(t, r, "TestCouponAppliesOnce", replay.Reproduced, "failed failed", "", 0)
	wantLines(t, r,
		"packages: ./cart",
		"✓ reproduced      TestCouponAppliesOnce  cart/cart_test.go",
		indent+"base: failed 2 of 2",
		indent+"base run 1: cart_test.go:7: Total(100, 10) = 80; want 90",
		"Tests: 1 reproduced.",
	)
	if len(r.report.HeadRuns) != 0 || len(r.report.Regressions) != 0 || strings.Contains(r.stdout, "TestNoDiscount") {
		t.Errorf("%d head runs and %d other tests; want none, because reproduce runs only the base commit and the named file", len(r.report.HeadRuns), len(r.report.Regressions))
	}
}

func TestReproduceSaysWhenATestPassesWithoutTheFix(t *testing.T) {
	repo, base := shop(t, fixedCart)
	repo.Commit(map[string]string{"cart/cart_test.go": couponTest})

	r := reproduceIn(t, repo.Dir, "--base", base, "--runs", "1", "cart/cart_test.go")

	wantVerdict(t, r, "TestCouponAppliesOnce", replay.NotReproduced, "passed", "", 1)
	wantLines(t, r, "✗ not reproduced  TestCouponAppliesOnce  cart/cart_test.go", "Tests: 1 not reproduced.")
}

func TestReproduceSaysWhenATestCannotRunWithoutTheFix(t *testing.T) {
	repo, base := shop(t, fixedCart)
	repo.Commit(map[string]string{
		"cart/free_test.go": "package cart\n\nimport \"testing\"\n\nfunc TestFreeShipping(t *testing.T) {\n" +
			"\tif !Free(50) {\n\t\tt.Error(\"an order of 50 ships for a fee\")\n\t}\n}\n",
	})

	r := reproduceIn(t, repo.Dir, "--base", base, "--runs", "1", "cart/free_test.go")

	wantVerdict(t, r, "TestFreeShipping", replay.CouldNotRun, "absent", "", 2)
	wantLines(t, r, "! could not run   TestFreeShipping  cart/free_test.go", indent+"base run 1: cart/free_test.go:6:6: undefined: Free")
}

func TestReproduceSaysWhenATestIsFlaky(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "counter")
	if err := os.WriteFile(counter, []byte("0"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COUNTER", counter)
	repo, _ := shop(t, fixedCart)
	base := repo.Commit(map[string]string{"elprovo.toml": strings.Replace(goConfig, `"GOMODCACHE"]`, `"GOMODCACHE", "COUNTER"]`, 1)})
	// The test fails in the first run and passes in the second.
	repo.Commit(map[string]string{"cart/counted_test.go": "package cart\n\nimport (\n\t\"os\"\n\t\"strconv\"\n\t\"testing\"\n)\n\n" +
		"func TestCounted(t *testing.T) {\n" +
		"\tdata, _ := os.ReadFile(os.Getenv(\"COUNTER\"))\n" +
		"\tn, _ := strconv.Atoi(string(data))\n" +
		"\tos.WriteFile(os.Getenv(\"COUNTER\"), []byte(strconv.Itoa(n+1)), 0o644)\n" +
		"\tif n == 0 {\n\t\tt.Fatal(\"first run\")\n\t}\n}\n"})

	r := reproduceIn(t, repo.Dir, "--base", base, "cart/counted_test.go")

	wantVerdict(t, r, "TestCounted", replay.Flaky, "failed passed", "", 1)
}

func TestReproduceRejectsFilesItCannotUse(t *testing.T) {
	repo, base := shop(t, buggyCart)
	repo.Commit(map[string]string{"cart/cart_test.go": couponTest})
	tests := []struct {
		name        string
		args        []string
		wantInError string
	}{
		{"no file", nil, "reproduce needs at least one test file"},
		{"a file that is not a Go test file", []string{"cart/cart.go"}, `"cart/cart.go" is not a Go test file`},
		{"a file that is not in the head commit", []string{"cart/missing_test.go"}, `"cart/missing_test.go" is not in the head commit`},
		{"a file whose tests did not change", []string{"cart/total_test.go"}, `"cart/total_test.go" has no test that the head commit`},
		{"a file outside the repository", []string{"../elsewhere_test.go"}, `"../elsewhere_test.go" is outside the repository`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := reproduceIn(t, repo.Dir, append([]string{"--base", base}, tt.args...)...)
			if r.status != 2 || !strings.Contains(r.stderr, tt.wantInError) {
				t.Errorf("exit %d, stderr %q; want 2 and an error that contains %q", r.status, r.stderr, tt.wantInError)
			}
			if _, err := os.Stat(filepath.Join(repo.Dir, ".elprovo")); err == nil {
				t.Error("an invalid reproduce made the output directory")
			}
		})
	}
}

func TestRequireFailsWhenNoTestIsFixed(t *testing.T) {
	repo, base := scriptRepo(t, scriptConfig("", "", ""), event("pass", "TestA"))

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")
	wantVerdict(t, r, "TestA", replay.Kept, "passed", "passed", 0)

	if err := os.RemoveAll(filepath.Join(repo.Dir, ".elprovo")); err != nil {
		t.Fatal(err)
	}
	r = replayIn(t, repo.Dir, "--base", base, "--runs", "1", "--require")
	if r.status != 1 {
		t.Errorf("with --require, exit %d; want 1\n%s%s", r.status, r.stdout, r.stderr)
	}
	wantLines(t, r, "No test is fixed, and --require is on, so this is a failure.")
}

func TestAChangeWithoutGoFilesHasNothingToTest(t *testing.T) {
	repo := testrepo.New(t)
	base := repo.Commit(testrepo.GoModule(map[string]string{"elprovo.toml": scriptConfig("", "", ""), "a.go": "package m\n"}))
	repo.Commit(map[string]string{"README": "docs\n", "testdata/case.go": "package fixture\n"})

	r := replayIn(t, repo.Dir, "--base", base)
	if r.status != 0 || !strings.Contains(r.stdout, "The change touches no Go package, so there is no test to run.\n") {
		t.Errorf("exit %d; want 0 and nothing to test\n%s%s", r.status, r.stdout, r.stderr)
	}

	r = replayIn(t, repo.Dir, "--base", base, "--require")
	if r.status != 1 {
		t.Errorf("with --require, exit %d; want 1\n%s%s", r.status, r.stdout, r.stderr)
	}
}

func TestDryRunListsTheChangedTestsAndRunsNoCommand(t *testing.T) {
	spy := t.TempDir()
	t.Setenv("SPY", spy)
	repo := testrepo.New(t)
	base := repo.Commit(testrepo.GoModule(map[string]string{
		"elprovo.toml": scriptConfig("pass_env = [\"SPY\"]\n", "", "start = 'touch \"$SPY/started\"'\nready = \"true\"\n"),
		"x_test.go":    "package m\n\nimport \"testing\"\n\nfunc TestKept(t *testing.T) {}\n\nfunc TestEdited(t *testing.T) {}\n",
	}))
	repo.Commit(map[string]string{"x_test.go": "package m\n\nimport \"testing\"\n\nfunc TestKept(t *testing.T) {}\n\nfunc TestEdited(t *testing.T) { t.Log(1) }\n\nfunc TestAdded(t *testing.T) {}\n"})

	r := replayIn(t, repo.Dir, "--base", base, "--dry-run")

	if r.status != 0 {
		t.Errorf("exit %d; want 0\n%s%s", r.status, r.stdout, r.stderr)
	}
	wantLines(t, r, "packages: .", "changed  TestAdded  x_test.go", "changed  TestEdited  x_test.go", "Dry run: no command ran.")
	if strings.Contains(r.stdout, "TestKept") {
		t.Errorf("stdout lists a test that did not change:\n%s", r.stdout)
	}
	if _, err := os.Stat(filepath.Join(spy, "started")); err == nil {
		t.Error("a dry run ran start")
	}
}

func TestNotFixedShowsTheFirstHeadRunThatFailed(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "counter")
	if err := os.WriteFile(counter, []byte("0"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COUNTER", counter)
	// The runs go base 1, head 1, base 2, head 2. TestA is skipped in head
	// run 1 and fails in the others.
	script := `n=$(cat "$COUNTER"); echo $((n + 1)) > "$COUNTER"
if [ "$n" -eq 1 ]; then
  printf '%s\n' '{"Action":"output","Package":"example.com/m","Test":"TestA","Output":"x_test.go:5: skipped\n"}' '{"Action":"skip","Package":"example.com/m","Test":"TestA"}'
else
  printf '%s\n' "{\"Action\":\"output\",\"Package\":\"example.com/m\",\"Test\":\"TestA\",\"Output\":\"x_test.go:5: run $n fails\\n\"}" '{"Action":"fail","Package":"example.com/m","Test":"TestA"}'
fi
`
	repo := testrepo.New(t)
	base := repo.Commit(testrepo.GoModule(map[string]string{"elprovo.toml": scriptConfig("pass_env = [\"COUNTER\"]\n", "", ""), "run.sh": script}))
	repo.Commit(map[string]string{"x_test.go": "package m\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n"})

	r := replayIn(t, repo.Dir, "--base", base)

	wantVerdict(t, r, "TestA", replay.NotFixed, "failed failed", "skipped failed", 1)
	wantLines(t, r, indent+"head run 2: x_test.go:5: run 3 fails", indent+"log: "+filepath.Join(".elprovo", "runs", r.replayID(t), "head-2", "test.log"))
}

func TestATestThatMovesToAnotherFileIsNotChanged(t *testing.T) {
	repo := testrepo.New(t)
	moved := "package m\n\nimport \"testing\"\n\nfunc TestMoved(t *testing.T) { t.Log(1) }\n"
	base := repo.Commit(testrepo.GoModule(map[string]string{"elprovo.toml": scriptConfig("", "", ""), "a_test.go": moved}))
	repo.Commit(map[string]string{"b_test.go": moved}, "a_test.go")

	r := replayIn(t, repo.Dir, "--base", base, "--dry-run")

	wantLines(t, r, "packages: .", "The change adds or edits no test.")
}

func TestAPackageInANestedModuleIsLeftOut(t *testing.T) {
	repo, base := scriptRepo(t, scriptConfig("", "", ""), event("pass", "TestA"))
	repo.Commit(map[string]string{"tools/go.mod": "module example.com/tools\n\ngo 1.27\n", "tools/x.go": "package tools\n", "tools/sub/y.go": "package sub\n"})

	r := replayIn(t, repo.Dir, "--base", base, "--dry-run")

	wantLines(t, r, "packages: .")
}

func TestTheExitStatusOfTheTestCommandDecidesNothing(t *testing.T) {
	repo, base := scriptRepo(t, scriptConfig("", "", ""), event("pass", "TestA")+"exit 1\n")

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantVerdict(t, r, "TestA", replay.Kept, "passed", "passed", 0)
	if code := r.report.HeadRuns[0].ExitCode; code == nil || *code != 1 {
		t.Errorf("exit_code is %v; want 1 in the report", code)
	}
}

func TestTestsThatPrintNoEventsStopTheReplay(t *testing.T) {
	repo, base := scriptRepo(t, scriptConfig("", "", ""), "echo 'go: cannot find main module'\nexit 1\n")

	r := replayIn(t, repo.Dir, "--base", base)

	if r.status != 2 || r.report.StoppedEarly != "base run 1 did not run: the tests printed no go test -json events" {
		t.Errorf("exit %d, stopped_early %q; want 2 and that base run 1 did not run\n%s", r.status, r.report.StoppedEarly, r.stdout)
	}
	wantLines(t, r,
		"The replay stopped early: base run 1 did not run: the tests printed no go test -json events.",
		"base run 1 ended with: go: cannot find main module",
		"log: "+filepath.Join(".elprovo", "runs", r.replayID(t), "base-1", "test.log"),
	)
	if len(r.report.HeadRuns) != 0 {
		t.Errorf("the report has %d head runs; want 0 after the replay stopped", len(r.report.HeadRuns))
	}
}

func TestManyListedTestsAreCut(t *testing.T) {
	var script strings.Builder
	for i := 1; i <= 25; i++ {
		script.WriteString(event("fail", fmt.Sprintf("TestF%02d", i)))
	}
	repo, base := scriptRepo(t, scriptConfig("", "", ""), script.String())

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantLines(t, r, "5 more of the other tests are not shown; report.json lists every test.", "Other tests: 25 already failing.")
	if !strings.Contains(r.stdout, "TestF20  .\n") || strings.Contains(r.stdout, "TestF21") {
		t.Errorf("stdout does not list the first 20 tests only:\n%s", r.stdout)
	}
}

func TestAnAppThatIsNeverReadyStopsTheReplay(t *testing.T) {
	repo, base := scriptRepo(t, scriptConfig("", "", "start = \"true\"\nready = \"echo connection refused; false\"\ntimeouts.ready = \"1s\"\n"), event("pass", "TestA"))

	r := replayIn(t, repo.Dir, "--base", base)

	if r.status != 2 || r.report.StoppedEarly != "base run 1 did not run: ready did not exit 0 within 1s" {
		t.Errorf("exit %d, stopped_early %q; want 2 and that ready did not exit 0", r.status, r.report.StoppedEarly)
	}
	if run := r.report.BaseRuns[0]; run.Reason != "ready did not exit 0 within 1s" || run.LastLine != "connection refused" {
		t.Errorf("reason %q, last_line %q; want the timeout and what ready printed", run.Reason, run.LastLine)
	}
	wantLines(t, r, "base run 1 ended with: connection refused")
}

func TestAFailingStartStopsTheReplayAndStopStillRuns(t *testing.T) {
	spy := t.TempDir()
	t.Setenv("SPY", spy)
	repo, base := scriptRepo(t, scriptConfig("pass_env = [\"SPY\"]\n", "", "start = \"echo cannot build; exit 4\"\nready = \"true\"\nstop = 'touch \"$SPY/stopped\"'\n"), event("pass", "TestA"))

	r := replayIn(t, repo.Dir, "--base", base)

	if r.status != 2 || r.report.StoppedEarly != "base run 1 did not run: start exited with status 4" {
		t.Errorf("exit %d, stopped_early %q; want 2 and that start failed", r.status, r.report.StoppedEarly)
	}
	wantLines(t, r,
		"base run 1 ended with: cannot build",
		"log: "+filepath.Join(".elprovo", "runs", r.replayID(t), "base-1", "start.log"),
	)
	if _, err := os.Stat(filepath.Join(spy, "stopped")); err != nil {
		t.Error("stop did not run after start failed")
	}
}

func TestReadyIsRunAgainUntilItSucceeds(t *testing.T) {
	// ready fails the first time and succeeds the second time.
	repo, base := scriptRepo(t, scriptConfig("", "", "start = \"true\"\nready = \"test -f tried || { touch tried; false; }\"\n"), event("pass", "TestA"))

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantVerdict(t, r, "TestA", replay.Kept, "passed", "passed", 0)
}

func TestATimedOutRunEndsItsProcessGroup(t *testing.T) {
	spy := t.TempDir()
	t.Setenv("SPY", spy)
	repo, base := scriptRepo(t, scriptConfig("pass_env = [\"SPY\"]\n", "timeout = \"1s\"\n", ""), "sleep 60 & echo $! > \"$SPY/pid\"; wait\n")

	started := time.Now()
	r := replayIn(t, repo.Dir, "--base", base)

	run := r.report.BaseRuns[0]
	if r.status != 2 || !run.TimedOut || run.ExitCode != nil || run.Reason != "the tests did not end within 1s and printed no go test -json events" {
		t.Errorf("exit %d, timed_out %v, exit_code %v, reason %q; want 2, true, null and the timeout", r.status, run.TimedOut, run.ExitCode, run.Reason)
	}
	if elapsed := time.Since(started); elapsed > 15*time.Second {
		t.Errorf("the replay took %v; the timeout is 1s", elapsed)
	}
	wantGone(t, filepath.Join(spy, "pid"))
}

func TestAProcessThatIgnoresSIGTERMIsKilled(t *testing.T) {
	spy := t.TempDir()
	t.Setenv("SPY", spy)
	// The child ignores SIGTERM, and the shell that started it does not.
	repo, base := scriptRepo(t, scriptConfig("pass_env = [\"SPY\"]\n", "timeout = \"1s\"\n", ""),
		"(trap '' TERM; exec sleep 300) & echo $! > \"$SPY/pid\"; wait\n")

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	if r.status != 2 {
		t.Errorf("exit %d; want 2", r.status)
	}
	wantGone(t, filepath.Join(spy, "pid"))
}

// wantGone checks that the process whose ID is in pidFile no longer runs.
func wantGone(t *testing.T, pidFile string) {
	t.Helper()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the command did not write its child's process ID: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); err == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("process %d still runs after its command timed out", pid)
	}
}

func TestAFailingStopEndsTheReplay(t *testing.T) {
	repo, base := scriptRepo(t, scriptConfig("", "", "start = \"true\"\nready = \"true\"\nstop = \"exit 3\"\n"), event("pass", "TestA"))

	r := replayIn(t, repo.Dir, "--base", base)

	if r.status != 2 || r.report.StoppedEarly != "stop exited with status 3" {
		t.Errorf("exit %d, stopped_early %q; want 2 and %q\n%s", r.status, r.report.StoppedEarly, "stop exited with status 3", r.stdout)
	}
	if len(r.report.BaseRuns) != 1 || len(r.report.HeadRuns) != 0 || r.test(t, "TestA").Verdict != replay.Unfinished {
		t.Errorf("%d base runs, %d head runs, verdict %q; want 1, 0 and no verdict", len(r.report.BaseRuns), len(r.report.HeadRuns), r.test(t, "TestA").Verdict)
	}
	wantLines(t, r, "Other tests: 1 without a verdict.")
}

func TestAnInterruptRunsStopAndEndsTheReplay(t *testing.T) {
	spy := t.TempDir()
	t.Setenv("SPY", spy)
	repo, base := scriptRepo(t, scriptConfig("pass_env = [\"SPY\"]\n", "", "start = \"true\"\nready = \"true\"\nstop = 'touch \"$SPY/stopped\"'\n"),
		"sleep 60 & echo $! > \"$SPY/pid\"; wait\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cancelWhen(ctx, cancel, filepath.Join(spy, "pid"))
	r := run(t, ctx, repo.Dir, "replay", "--base", base)

	if r.status != 2 || r.report.StoppedEarly != "interrupted" {
		t.Errorf("exit %d, stopped_early %q; want 2 and interrupted\n%s%s", r.status, r.report.StoppedEarly, r.stdout, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(spy, "stopped")); err != nil {
		t.Error("stop did not run after the interrupt")
	}
	if len(r.report.BaseRuns) != 0 {
		t.Errorf("the report has %d base runs; the interrupted run must not count", len(r.report.BaseRuns))
	}
	wantGone(t, filepath.Join(spy, "pid"))
}

// cancelWhen calls cancel when the file at path exists.
func cancelWhen(ctx context.Context, cancel context.CancelFunc, path string) {
	for {
		if _, err := os.Stat(path); err == nil {
			cancel()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestAnInterruptDuringTheCopyIsReportedAsAnInterrupt(t *testing.T) {
	spy := t.TempDir()
	t.Setenv("SPY", spy)
	repo := testrepo.New(t)
	// A smudge filter that waits makes the copy slow enough to interrupt.
	repo.Git("config", "filter.slow.smudge", `touch "$SPY/copying"; sleep 30; cat`)
	base := repo.Commit(testrepo.GoModule(map[string]string{
		"elprovo.toml": scriptConfig("", "", ""), "run.sh": event("pass", "TestA"), "a.go": "package m\n",
		".gitattributes": "slow.txt filter=slow\n", "slow.txt": "data\n",
	}))
	repo.Commit(map[string]string{"a.go": "package m\n\nconst A = 1\n"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cancelWhen(ctx, cancel, filepath.Join(spy, "copying"))
	r := run(t, ctx, repo.Dir, "replay", "--base", base)

	if r.status != 2 || r.report.StoppedEarly != "interrupted" {
		t.Errorf("exit %d, stopped_early %q; want 2 and interrupted\n%s%s", r.status, r.report.StoppedEarly, r.stdout, r.stderr)
	}
}

func TestCommandsGetOnlyTheRunsEnvironmentAndACopyWithTheHeadTests(t *testing.T) {
	spy := t.TempDir()
	t.Setenv("SPY", spy)
	t.Setenv("PARENT_SECRET", "must not leak")
	t.Setenv("LANG", "en_US.UTF-8")
	script := "d=\"$SPY/$ELPROVO_RUN_ID\"; mkdir \"$d\"\n" +
		"env | sort > \"$d/env\"; pwd > \"$d/pwd\"; ls -A \"$HOME\" > \"$d/home\"\n" +
		"find . -type f | sort > \"$d/files\"; cat a.go kept_test.go > \"$d/content\"\n" +
		"echo \"$ELPROVO_RUN_ID\" > \"$ELPROVO_ARTIFACTS/seen.txt\"\n" + event("pass", "TestA")
	repo := testrepo.New(t)
	base := repo.Commit(testrepo.GoModule(map[string]string{
		"elprovo.toml": scriptConfig("pass_env = [\"SPY\"]\nenv.FROM_ENV = \"plain\"\n", "", ""),
		"run.sh":       script,
		"a.go":         "package m\n// base code\n",
		"kept_test.go": "package m\n// base test\n",
		"old_test.go":  "package m\n",
	}))
	repo.Commit(map[string]string{
		"a.go":         "package m\n// head code\n",
		"kept_test.go": "package m\n// head test\n",
		"new_test.go":  "package m\n",
	}, "old_test.go")

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")
	if r.status != 0 {
		t.Fatalf("exit %d\n%s%s", r.status, r.stdout, r.stderr)
	}

	wantContent := map[string]string{"base-1": "package m\n// base code\npackage m\n// head test\n", "head-1": "package m\n// head code\npackage m\n// head test\n"}
	homes := map[string]bool{}
	for _, run := range append(r.report.BaseRuns, r.report.HeadRuns...) {
		// The run's artifact holds its run ID, which names its records.
		seen, err := os.ReadFile(filepath.Join(repo.Dir, ".elprovo", "runs", r.replayID(t), run.Dir, "artifacts", "seen.txt"))
		if err != nil {
			t.Fatalf("the %s artifact is not in the output directory: %v", run.Dir, err)
		}
		d := filepath.Join(spy, strings.TrimSpace(string(seen)))
		vars := map[string]string{}
		for _, line := range readLines(t, filepath.Join(d, "env")) {
			name, value, _ := strings.Cut(line, "=")
			vars[name] = value
		}
		if vars["HOME"] == "" || vars["HOME"] == os.Getenv("HOME") || homes[vars["HOME"]] {
			t.Errorf("HOME is %q; want a new directory for each run", vars["HOME"])
		}
		homes[vars["HOME"]] = true
		if vars["TMPDIR"] == "" || vars["TMPDIR"] == os.Getenv("TMPDIR") {
			t.Errorf("TMPDIR is %q; want the run's own directory", vars["TMPDIR"])
		}
		if vars["FROM_ENV"] != "plain" || vars["SPY"] != spy || vars["ELPROVO_RUN_ID"] != filepath.Base(d) {
			t.Errorf("env, pass_env or ELPROVO_RUN_ID is wrong: %v", vars)
		}
		if _, ok := vars["PARENT_SECRET"]; ok {
			t.Error("a variable from the environment of elprovo replay leaked into the run")
		}
		if vars["LANG"] != "en_US.UTF-8" || vars["USER"] != os.Getenv("USER") {
			t.Errorf("LANG is %q and USER is %q; want them from the environment of elprovo replay", vars["LANG"], vars["USER"])
		}
		if artifacts := vars["ELPROVO_ARTIFACTS"]; strings.Contains(artifacts, "base") || strings.Contains(artifacts, "head") || strings.Contains(artifacts, ".elprovo") {
			t.Errorf("ELPROVO_ARTIFACTS is %q; it must not show the commit or lead to the output directory", artifacts)
		}
		for name := range vars {
			if strings.HasPrefix(name, "ELPROVO_") && name != "ELPROVO_RUN_ID" && name != "ELPROVO_ARTIFACTS" {
				t.Errorf("the run got %s; only ELPROVO_RUN_ID and ELPROVO_ARTIFACTS are allowed", name)
			}
		}
		if pwd := readLines(t, filepath.Join(d, "pwd")); len(pwd) != 1 || filepath.Base(pwd[0]) != "elprovo-"+filepath.Base(d) {
			t.Errorf("the working directory is %v; want elprovo-%s", pwd, filepath.Base(d))
		}
		if home := readLines(t, filepath.Join(d, "home")); len(home) != 0 {
			t.Errorf("HOME holds %v; want it empty", home)
		}
		if files := strings.Join(readLines(t, filepath.Join(d, "files")), " "); files != "./a.go ./elprovo.toml ./go.mod ./kept_test.go ./new_test.go ./run.sh" {
			t.Errorf("the %s copy holds %q; want the files of its commit, with the head commit's tests", run.Dir, files)
		}
		if content, _ := os.ReadFile(filepath.Join(d, "content")); string(content) != wantContent[run.Dir] {
			t.Errorf("the %s copy has a.go and kept_test.go %q; want %q", run.Dir, content, wantContent[run.Dir])
		}
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func TestBaseRunsUseTheBaseConfigAndTheCommitsAlternate(t *testing.T) {
	spy := t.TempDir()
	t.Setenv("SPY", spy)
	repo := testrepo.New(t)
	base := repo.Commit(testrepo.GoModule(map[string]string{
		"elprovo.toml": scriptConfig("pass_env = [\"SPY\"]\n", "", "start = 'echo base >> \"$SPY/order\"'\nready = \"true\"\n"),
		"run.sh":       event("pass", "TestA"), "a.go": "package m\n",
	}))
	repo.Commit(map[string]string{
		"elprovo.toml": scriptConfig("pass_env = [\"SPY\"]\n", "", "start = 'echo head >> \"$SPY/order\"'\nready = \"true\"\n"),
		"a.go":         "package m\n\nconst A = 1\n",
	})

	r := replayIn(t, repo.Dir, "--base", base)

	wantVerdict(t, r, "TestA", replay.Kept, "passed passed", "passed passed", 0)
	if order := strings.Join(readLines(t, filepath.Join(spy, "order")), " "); order != "base head base head" {
		t.Errorf("the runs started in the order %q; want %q", order, "base head base head")
	}
}

func TestABaseConfigOfVersion1GivesWayToTheHeadConfig(t *testing.T) {
	spy := t.TempDir()
	t.Setenv("SPY", spy)
	repo := testrepo.New(t)
	base := repo.Commit(testrepo.GoModule(map[string]string{
		"elprovo.toml": "version = 1\nstart = 'echo version 1 >> \"$SPY/order\"'\nready = \"true\"\npass_env = [\"SPY\"]\n",
		"run.sh":       event("pass", "TestA"), "a.go": "package m\n",
	}))
	repo.Commit(map[string]string{
		"elprovo.toml": scriptConfig("pass_env = [\"SPY\"]\n", "", "start = 'echo version 2 >> \"$SPY/order\"'\nready = \"true\"\n"),
		"a.go":         "package m\n\nconst A = 1\n",
	})

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantVerdict(t, r, "TestA", replay.Kept, "passed", "passed", 0)
	if order := strings.Join(readLines(t, filepath.Join(spy, "order")), ", "); order != "version 2, version 2" {
		t.Errorf("the runs started in the order %q; want the head commit's start for both", order)
	}
}

func TestTheDefaultBaseIsTheMergeBaseWithOriginHead(t *testing.T) {
	repo := testrepo.New(t)
	fork := repo.Commit(testrepo.GoModule(map[string]string{"elprovo.toml": scriptConfig("", "", ""), "run.sh": event("pass", "TestA"), "a.go": "package m\n"}))
	repo.Git("switch", "-q", "-c", "change")
	repo.Commit(map[string]string{"a.go": "package m\n\nconst A = 1\n"})
	// main moves on after the branch was made.
	repo.Git("switch", "-q", "main")
	upstream := repo.Commit(map[string]string{"README": "later on main\n"})
	repo.Git("switch", "-q", "change")
	repo.Git("update-ref", "refs/remotes/origin/main", upstream)
	repo.Git("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")

	r := replayIn(t, repo.Dir, "--runs", "1")

	if r.report.Base != fork {
		t.Errorf("the base commit is %s; want the merge base %s, not origin/HEAD %s", r.report.Base, fork, upstream)
	}
	wantVerdict(t, r, "TestA", replay.Kept, "passed", "passed", 0)
}

func TestOutIsRelativeToTheWorkingDirectory(t *testing.T) {
	repo, base := scriptRepo(t, scriptConfig("", "", ""), event("pass", "TestA"))
	sub := filepath.Join(repo.Dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	status := Run(context.Background(), "test", []string{"replay", "--base", base, "--runs", "1", "--out", "../out"},
		Env{Stdout: &stdout, Stderr: &stderr, Environ: os.Environ(), Dir: sub})

	if status != 0 {
		t.Errorf("exit %d; want 0\n%s%s", status, stdout.String(), stderr.String())
	}
	if reports, _ := filepath.Glob(filepath.Join(repo.Dir, "out", "runs", "*", "report.json")); len(reports) != 1 {
		t.Errorf("found %d reports in the out directory; want 1", len(reports))
	}
}

func TestTheCopyAppliesCheckoutFilters(t *testing.T) {
	repo := testrepo.New(t)
	// Git LFS fills files through a checkout filter like this one, which
	// writes the stored text in capitals.
	repo.Git("config", "filter.upper.clean", "cat")
	repo.Git("config", "filter.upper.smudge", "tr a-z A-Z")
	base := repo.Commit(testrepo.GoModule(map[string]string{
		"elprovo.toml": scriptConfig("", "", ""), "a.go": "package m\n",
		".gitattributes": "data.txt filter=upper\n", "data.txt": "hello\n",
		"run.sh": "if grep -qx HELLO data.txt; then\n" + event("pass", "TestFiltered") + "else\n" + event("fail", "TestFiltered") + "fi\n",
	}))
	repo.Commit(map[string]string{"a.go": "package m\n\nconst A = 1\n"})

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantVerdict(t, r, "TestFiltered", replay.Kept, "passed", "passed", 0)
}

func TestAGitLFSFileIsCopiedWithItsContent(t *testing.T) {
	if err := exec.Command("git", "lfs", "version").Run(); err != nil {
		t.Skip("git-lfs is not installed")
	}
	repo := testrepo.New(t)
	repo.Git("lfs", "install", "--local")
	repo.Git("lfs", "track", "data.bin")
	base := repo.Commit(testrepo.GoModule(map[string]string{
		"elprovo.toml": scriptConfig("", "", ""), "a.go": "package m\n", "data.bin": "real content\n",
		"run.sh": "if grep -qx 'real content' data.bin; then\n" + event("pass", "TestLFS") + "else\n" + event("fail", "TestLFS") + "fi\n",
	}))
	if pointer := repo.Git("cat-file", "-p", base+":data.bin"); !strings.Contains(pointer, "git-lfs.github.com/spec") {
		t.Fatalf("the commit holds %q; want a Git LFS pointer", pointer)
	}
	repo.Commit(map[string]string{"a.go": "package m\n\nconst A = 1\n"})

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantVerdict(t, r, "TestLFS", replay.Kept, "passed", "passed", 0)
}

func TestInvalidInputExitsTwoWithoutRunning(t *testing.T) {
	valid := scriptConfig("", "", "")
	tests := []struct {
		name        string
		files       map[string]string
		args        []string
		wantInError string
	}{
		{"unknown field", map[string]string{"elprovo.toml": valid + "stopp = \"x\"\n"}, nil, `unknown field "tests.stopp"`},
		{"version 1", map[string]string{"elprovo.toml": "version = 1\nstart = \"true\"\nready = \"true\"\n"}, nil, "version 1 is the proof-file format of Elprovo 0.2"},
		{"reserved pass_env name", map[string]string{"elprovo.toml": scriptConfig("pass_env = [\"ELPROVO_TEST_UNSET\"]\n", "", "")}, nil, "must not start with ELPROVO_"},
		{"unset pass_env variable", map[string]string{"elprovo.toml": scriptConfig("pass_env = [\"UNSET_FOR_ELPROVO_TEST\"]\n", "", "")}, nil, "pass_env names UNSET_FOR_ELPROVO_TEST, which is not set"},
		{"no elprovo.toml", map[string]string{"elprovo.toml": ""}, nil, "has no elprovo.toml"},
		{"no go.mod", map[string]string{"elprovo.toml": valid, "go.mod": ""}, nil, "has no go.mod with a module line"},
		{"a test file that is not Go", map[string]string{"elprovo.toml": valid, "x_test.go": "package m\nfunc TestA(t *testing.T) {"}, nil, "x_test.go is not valid Go"},
		{"too many runs", map[string]string{"elprovo.toml": valid}, []string{"--runs", "11"}, "--runs is 11"},
		{"an argument", map[string]string{"elprovo.toml": valid}, []string{"proofs/feature.toml"}, `replay takes no arguments, but got "proofs/feature.toml"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := testrepo.New(t)
			base := repo.Commit(map[string]string{"README": "base\n"})
			files := testrepo.GoModule(map[string]string{"a.go": "package m\n"})
			// An empty content leaves the file out.
			for name, content := range tt.files {
				files[name] = content
				if content == "" {
					delete(files, name)
				}
			}
			repo.Commit(files)
			r := replayIn(t, repo.Dir, append([]string{"--base", base}, tt.args...)...)
			if r.status != 2 || !strings.Contains(r.stderr, tt.wantInError) {
				t.Errorf("exit %d, stderr %q; want 2 and an error that contains %q", r.status, r.stderr, tt.wantInError)
			}
			if _, err := os.Stat(filepath.Join(repo.Dir, ".elprovo")); err == nil {
				t.Error("an invalid replay made the output directory")
			}
		})
	}
}

func TestVersionPrintsTheVersion(t *testing.T) {
	r := run(t, context.Background(), t.TempDir(), "version")
	if r.status != 0 || r.stdout != "elprovo test\n" {
		t.Errorf("exit %d, stdout %q; want 0 and %q", r.status, r.stdout, "elprovo test\n")
	}
}

func TestControlCharactersNeverReachTheTerminal(t *testing.T) {
	t.Run("in an unknown field", func(t *testing.T) {
		repo := testrepo.New(t)
		base := repo.Commit(map[string]string{"README": "base\n"})
		repo.Commit(testrepo.GoModule(map[string]string{"a.go": "package m\n", "elprovo.toml": scriptConfig("", "\"\\e]52;c;owned\\u0007\" = \"x\"\n", "")}))

		r := replayIn(t, repo.Dir, "--base", base, "--dry-run")

		wantNoControl(t, r, 2)
	})
	t.Run("in a package name and a message", func(t *testing.T) {
		repo := testrepo.New(t)
		script := `printf '%s\n' '{"Action":"output","Package":"` + testrepo.Module + `","Test":"TestA","Output":"gone\u001b[2K\n"}'` + "\n" + event("fail", "TestA")
		base := repo.Commit(testrepo.GoModule(map[string]string{"elprovo.toml": scriptConfig("", "", ""), "run.sh": script, "a.go": "package m\n"}))
		repo.Commit(map[string]string{"a.go": "package m\n\nconst A = 1\n", "x\x1b[2Ky/b.go": "package y\n"})

		r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

		wantNoControl(t, r, 0)
		if got := r.test(t, "TestA").Head[0].Message; got != `gone\x1b[2K` {
			t.Errorf("the message is %q; want %q", got, `gone\x1b[2K`)
		}
	})
}

// emptyTest is a test file in the root package with one empty test, name.
func emptyTest(name string) string {
	return "package m\n\nimport \"testing\"\n\nfunc " + name + "(t *testing.T) {}\n"
}

func TestHeadNamesTheCommitWithTheChange(t *testing.T) {
	repo := testrepo.New(t)
	base := repo.Commit(testrepo.GoModule(map[string]string{"elprovo.toml": scriptConfig("", "", ""), "run.sh": event("pass", "TestA"), "a.go": "package m\n"}))
	head := repo.Commit(map[string]string{"a.go": "package m\n\nconst A = 1\n"})
	repo.Commit(map[string]string{"run.sh": event("fail", "TestA")})

	r := replayIn(t, repo.Dir, "--base", base, "--head", head, "--runs", "1")

	if r.report.Head != head {
		t.Errorf("the head commit is %s; want %s", r.report.Head, head)
	}
	wantVerdict(t, r, "TestA", replay.Kept, "passed", "passed", 0)
}

func TestReproduceReadsTheFilesFromTheHeadCommit(t *testing.T) {
	repo := testrepo.New(t)
	base := repo.Commit(testrepo.GoModule(map[string]string{"elprovo.toml": scriptConfig("", "", ""), "run.sh": event("fail", "TestA"), "a.go": "package m\n"}))
	head := repo.Commit(map[string]string{"a_test.go": emptyTest("TestA")})
	repo.Commit(nil, "a_test.go")

	r := reproduceIn(t, repo.Dir, "--base", base, "--head", head, "--runs", "1", "a_test.go")

	wantVerdict(t, r, "TestA", replay.Reproduced, "failed", "", 0)
}

func TestReproduceJudgesOnlyTheNamedFilesOnce(t *testing.T) {
	repo := testrepo.New(t)
	base := repo.Commit(testrepo.GoModule(map[string]string{"elprovo.toml": scriptConfig("", "", ""), "run.sh": event("fail", "TestA") + event("pass", "TestB"), "a.go": "package m\n"}))
	repo.Commit(map[string]string{"a_test.go": emptyTest("TestA"), "b_test.go": emptyTest("TestB")})

	r := reproduceIn(t, repo.Dir, "--base", base, "--runs", "1", "a_test.go", "./a_test.go")

	wantVerdict(t, r, "TestA", replay.Reproduced, "failed", "", 0)
	if len(r.report.Changed) != 1 {
		t.Errorf("the report has %d changed tests; want TestA once", len(r.report.Changed))
	}
}

func TestTheBaseConfigIsCheckedToo(t *testing.T) {
	for _, tt := range []struct {
		name, baseConfig, wantInError string
	}{
		{"an unknown field in version 2", scriptConfig("", "stopp = \"x\"\n", ""), "elprovo.toml in the base commit"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := testrepo.New(t)
			base := repo.Commit(testrepo.GoModule(map[string]string{"elprovo.toml": tt.baseConfig, "run.sh": event("pass", "TestA"), "a.go": "package m\n"}))
			repo.Commit(map[string]string{"elprovo.toml": scriptConfig("", "", ""), "a.go": "package m\n\nconst A = 1\n"})

			r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

			if r.status != 2 || !strings.Contains(r.stderr, tt.wantInError) {
				t.Errorf("exit %d, stderr %q; want 2 and an error that contains %q", r.status, r.stderr, tt.wantInError)
			}
		})
	}
}

func TestAChangeThatOnlyDeletesAGoFileTestsItsPackage(t *testing.T) {
	repo := testrepo.New(t)
	base := repo.Commit(testrepo.GoModule(map[string]string{"elprovo.toml": scriptConfig("", "", ""), "run.sh": event("pass", "TestA"), "a.go": "package m\n", "b.go": "package m\n"}))
	repo.Commit(map[string]string{"run.sh": event("fail", "TestA")}, "b.go")

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantVerdict(t, r, "TestA", replay.Broke, "passed", "failed", 1)
}

func TestAMovedGoFileTouchesBothPackages(t *testing.T) {
	repo := testrepo.New(t)
	body := "package a\n\n// X is a constant with a body long enough for Git to find the rename.\nconst X = 1\n"
	base := repo.Commit(testrepo.GoModule(map[string]string{"elprovo.toml": scriptConfig("", "", ""), "a/x.go": body}))
	repo.Commit(map[string]string{"b/x.go": body}, "a/x.go")

	r := replayIn(t, repo.Dir, "--base", base, "--dry-run")

	wantLines(t, r, "packages: ./a ./b")
}

func TestAPackageThatTheChangeDeletesIsNotRunWithAReason(t *testing.T) {
	repo := testrepo.New(t)
	script := `printf '%s\n' '{"Action":"pass","Package":"example.com/m/old","Test":"TestOld"}'` + "\n"
	cfg := strings.Replace(scriptConfig("", "", ""), `files = ["**/*_test.go"]`, `files = ["e2e/**"]`, 1)
	base := repo.Commit(testrepo.GoModule(map[string]string{"elprovo.toml": cfg, "run.sh": script, "old/o.go": "package old\n"}))
	repo.Commit(nil, "old")

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantVerdict(t, r, "TestOld", replay.NotRun, "passed", "absent", 0)
	if got := r.test(t, "TestOld").Head[0].Message; got != "the head commit has no Go files in ./old" {
		t.Errorf("the head message is %q; want that the head commit has no Go files there", got)
	}
}

func TestATimedOutRunKeepsTheResultsItPrinted(t *testing.T) {
	repo := testrepo.New(t)
	cfg := scriptConfig("", "timeout = \"1s\"\n", "")
	base := repo.Commit(testrepo.GoModule(map[string]string{"elprovo.toml": cfg, "run.sh": event("pass", "TestA") + event("pass", "TestB"), "a.go": "package m\n"}))
	repo.Commit(map[string]string{"run.sh": event("pass", "TestA") + "sleep 60\n", "a.go": "package m\n\nconst A = 1\n"})

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantVerdict(t, r, "TestA", replay.Kept, "passed", "passed", 1)
	wantVerdict(t, r, "TestB", replay.Broke, "passed", "absent", 1)
	if run := r.report.HeadRuns[0]; run.Outcome != replay.Ran || !run.TimedOut || run.ExitCode != nil {
		t.Errorf("head run %+v; want ran, timed out, and no exit code", run)
	}
	if got := r.test(t, "TestB").Head[0].Message; got != "the tests did not end within 1s" {
		t.Errorf("the head message of TestB is %q; want the timeout", got)
	}
}

func TestAGoTestFileIsATestFileEvenWhenTestsFilesDoesNotMatchIt(t *testing.T) {
	repo := testrepo.New(t)
	cfg := strings.Replace(scriptConfig("", "", ""), `files = ["**/*_test.go"]`, `files = ["e2e/**"]`, 1)
	// TestKept passes only on the head commit's kept_test.go.
	script := "if grep -q head kept_test.go; then\n" + event("pass", "TestKept") + "else\n" + event("fail", "TestKept") + "fi\n" + event("pass", "TestNew")
	base := repo.Commit(testrepo.GoModule(map[string]string{"elprovo.toml": cfg, "run.sh": script, "kept_test.go": "package m\n"}))
	repo.Commit(map[string]string{"kept_test.go": "package m\n\n// head\n", "new_test.go": emptyTest("TestNew")})

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantVerdict(t, r, "TestKept", replay.Kept, "passed", "passed", 1)
	wantVerdict(t, r, "TestNew", replay.NotReproduced, "passed", "passed", 1)
}

func TestBaseRunsRunTheTestsAsTheHeadConfigSays(t *testing.T) {
	repo := testrepo.New(t)
	baseConfig := strings.Replace(scriptConfig("", "", ""), "sh run.sh {packages}", "sh other.sh {packages}", 1)
	base := repo.Commit(testrepo.GoModule(map[string]string{
		"elprovo.toml": baseConfig, "run.sh": event("pass", "TestA"), "other.sh": event("fail", "TestA"), "a.go": "package m\n",
	}))
	repo.Commit(map[string]string{"elprovo.toml": scriptConfig("", "", ""), "a.go": "package m\n\nconst A = 1\n"})

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantVerdict(t, r, "TestA", replay.Kept, "passed", "passed", 0)
}

func TestAChangedTestNamedInTwoFilesOfItsPackageIsAnError(t *testing.T) {
	repo := testrepo.New(t)
	external := "package m_test\n\nimport \"testing\"\n\nfunc TestTotal(t *testing.T) {}\n"
	base := repo.Commit(testrepo.GoModule(map[string]string{"elprovo.toml": scriptConfig("", "", ""), "a.go": "package m\n", "b_test.go": external}))
	repo.Commit(map[string]string{"a_test.go": "package m\n\nimport \"testing\"\n\nfunc TestTotal(t *testing.T) { t.Log(1) }\n"})

	r := replayIn(t, repo.Dir, "--base", base, "--dry-run")

	if r.status != 2 || !strings.Contains(r.stderr, "a_test.go: TestTotal is declared in b_test.go too") {
		t.Errorf("exit %d, stderr %q; want 2 and an error that names both files", r.status, r.stderr)
	}
}

func TestTheBaseCopyTakesTheTestFilesThatTheHeadConfigNames(t *testing.T) {
	repo := testrepo.New(t)
	script := "if grep -qx new testdata/case.txt; then\n" + event("pass", "TestFixture") + "else\n" + event("fail", "TestFixture") + "fi\n"
	base := repo.Commit(testrepo.GoModule(map[string]string{"elprovo.toml": scriptConfig("", "", ""), "run.sh": script, "a.go": "package m\n", "testdata/case.txt": "old\n"}))
	headConfig := strings.Replace(scriptConfig("", "", ""), `files = ["**/*_test.go"]`, `files = ["**/*_test.go", "**/testdata/**"]`, 1)
	repo.Commit(map[string]string{"elprovo.toml": headConfig, "testdata/case.txt": "new\n", "a.go": "package m\n\nconst A = 1\n"})

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantVerdict(t, r, "TestFixture", replay.Kept, "passed", "passed", 0)
}

func TestTheOutputListsFixedAndFlakyOtherTests(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "seen")
	t.Setenv("COUNTER", counter)
	repo := testrepo.New(t)
	base := repo.Commit(testrepo.GoModule(map[string]string{
		"elprovo.toml": scriptConfig("pass_env = [\"COUNTER\"]\n", "", ""), "a.go": "package m\n",
		"run.sh": event("fail", "TestFixed") + event("pass", "TestFlaky"),
	}))
	// TestFlaky passes in head run 1 and fails in head run 2.
	repo.Commit(map[string]string{
		"a.go":   "package m\n\nconst A = 1\n",
		"run.sh": event("pass", "TestFixed") + "if [ -f \"$COUNTER\" ]; then\n" + event("fail", "TestFlaky") + "else\ntouch \"$COUNTER\"\n" + event("pass", "TestFlaky") + "fi\n",
	})

	r := replayIn(t, repo.Dir, "--base", base)

	wantLines(t, r, "✓ fixed           TestFixed  .", "✗ flaky           TestFlaky  .")
}

func TestReportJSONHasTheFieldsOfFormat2(t *testing.T) {
	repo, base := scriptRepo(t, scriptConfig("", "", ""), event("pass", "TestA"))
	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	data, err := os.ReadFile(filepath.Join(repo.Dir, ".elprovo", "runs", r.replayID(t), "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for key := range report {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	want := []string{"base", "base_runs", "changed", "command", "format", "head", "head_runs", "packages", "regressions", "runs", "stopped_early"}
	if !slices.Equal(keys, want) || report["format"] != float64(2) {
		t.Errorf("report.json has the fields %v and format %v; want %v and 2", keys, report["format"], want)
	}
}

func TestARunThatDidNotRunShowsItsLastLine(t *testing.T) {
	t.Run("escaped", func(t *testing.T) {
		repo, base := scriptRepo(t, scriptConfig("", "", ""), "printf 'gone\\033[2K\\n'\nexit 1\n")
		r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")
		wantNoControl(t, r, 2)
		wantLines(t, r, `base run 1 ended with: gone\x1b[2K`)
	})
	t.Run("empty", func(t *testing.T) {
		repo, base := scriptRepo(t, scriptConfig("", "", ""), "exit 1\n")
		r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")
		wantLines(t, r, "base run 1 printed nothing.")
	})
	t.Run("on the head commit", func(t *testing.T) {
		repo := testrepo.New(t)
		base := repo.Commit(testrepo.GoModule(map[string]string{"elprovo.toml": scriptConfig("", "", ""), "run.sh": event("pass", "TestA"), "a.go": "package m\n"}))
		repo.Commit(map[string]string{"run.sh": "echo head broke\n", "a.go": "package m\n\nconst A = 1\n"})
		r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")
		if r.status != 2 {
			t.Errorf("exit %d; want 2", r.status)
		}
		wantLines(t, r, "head run 1 ended with: head broke")
	})
}

func TestTextFromAChangeCannotAddLines(t *testing.T) {
	repo, base := scriptRepo(t, scriptConfig("", "", ""), event("pass", "TestA"))
	repo.Commit(map[string]string{"x\n✓ fixed           TestForged  forged_test.go\ny/b.go": "package y\n"})

	r := replayIn(t, repo.Dir, "--base", base, "--dry-run")

	if strings.Contains(r.stdout, "\n✓ fixed") {
		t.Errorf("a directory name added a line to the output:\n%s", r.stdout)
	}
}

func TestAnErrorAboutAFileFromAChangeIsEscaped(t *testing.T) {
	repo, base := scriptRepo(t, scriptConfig("", "", ""), event("pass", "TestA"))
	// A test file over the size limit makes an error that names the file.
	repo.Commit(map[string]string{"a\x1b[2J_test.go": "package m\n//" + strings.Repeat("x", 4<<20) + "\n"})

	r := replayIn(t, repo.Dir, "--base", base, "--dry-run")

	wantNoControl(t, r, 2)
	if !strings.Contains(r.stderr, `a\x1b[2J_test.go`) {
		t.Errorf("stderr does not name the file with its control character escaped: %q", r.stderr)
	}
}

func wantNoControl(t *testing.T, r result, wantStatus int) {
	t.Helper()
	if r.status != wantStatus {
		t.Errorf("exit %d; want %d\n%s%s", r.status, wantStatus, r.stdout, r.stderr)
	}
	for _, out := range []string{r.stdout, r.stderr} {
		if strings.ContainsAny(out, "\x1b\a") {
			t.Errorf("a control character reached the output: %q", out)
		}
	}
}
