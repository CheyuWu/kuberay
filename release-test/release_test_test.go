package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBlobSHA(t *testing.T) {
	// The values `git hash-object` prints for the same content.
	assert.Equal(t, "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391", blobSHA(nil))
	assert.Equal(t, "ce013625030ba8dba906f756967f9e9ca394464a", blobSHA([]byte("hello\n")))
}

func TestDocsPagePath(t *testing.T) {
	tests := []struct {
		name, requested, final, page string
		ok                           bool
	}{
		{
			name:      "moved and renamed page",
			requested: docsMaster + "cluster/kubernetes/user-guides/kuberay-gcs-ft.html",
			final:     docsMaster + "kuberay/user-guides/gcs-ft.html",
			page:      "kuberay/user-guides/gcs-ft",
			ok:        true,
		},
		{
			name:      "removed page redirects to the section index",
			requested: docsMaster + "cluster/kubernetes/examples/ml-example.html",
			final:     docsMaster + "kuberay/index.html",
			page:      "kuberay/index",
		},
		{
			name:      "an index page asked for by name",
			requested: docsMaster + "kuberay/examples/index.html",
			final:     docsMaster + "kuberay/examples/index.html",
			page:      "kuberay/examples/index",
			ok:        true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			page, ok := docsPagePath(tc.requested, tc.final)
			assert.Equal(t, tc.page, page)
			assert.Equal(t, tc.ok, ok)
		})
	}
}

func TestLintManifest(t *testing.T) {
	items := []Item{
		{ID: "doc", Source: "https://docs.ray.io/en/master/kuberay/x.html"},
		{ID: "sample", Source: "ray-operator/config/samples/x.yaml"},
		{ID: "gpu", Source: "ray-operator/config/samples/gpu.yaml", Requires: []string{"gpu"}},
	}
	step := func(run string) []Step { return []Step{{Run: run, Check: "true"}} }
	tests := []struct {
		name  string
		units []Unit
		want  []string // substrings, one per expected problem
	}{
		{
			name: "valid",
			units: []Unit{
				{ID: "doc", SourceSHA: "a", Steps: step("helm install kuberay-operator kuberay/kuberay-operator --version $CHART_VERSION")},
				{ID: "sample", SourceSHA: "b", CoveredBy: "doc"},
			},
		},
		{
			name: "structure",
			units: []Unit{
				{ID: "doc", Steps: []Step{{Check: "true"}}},
				{ID: "doc", SourceSHA: "a", Steps: step("true")},
				{ID: "sample", SourceSHA: "b", CoveredBy: "nope"},
				{ID: "gpu", SourceSHA: "c", Steps: step("true")},
				{ID: "unknown", SourceSHA: "d"},
			},
			want: []string{
				"doc: duplicate unit", "doc: no source_sha", "doc: step 1 has no run",
				"sample: covered_by nope", "gpu: the Item has requires",
				"unknown: not in items.yaml", "unknown: no steps and no covered_by",
			},
		},
		{
			name: "versions",
			units: []Unit{{ID: "doc", SourceSHA: "a", Steps: []Step{
				{Run: "kubectl apply -f https://raw.githubusercontent.com/ray-project/kuberay/master/x.yaml"},
				{Run: "kubectl apply -f https://raw.githubusercontent.com/ray-project/kuberay/v1.7.0/x.yaml"},
				{Run: "kubectl create -k \"github.com/ray-project/kuberay/ray-operator/config/default?ref=v1.5.1\""},
				{Run: "helm install kuberay-operator kuberay/kuberay-operator \\\n  --version 1.5.1"},
				{Run: "kubectl set image deploy/kuberay-operator kuberay-operator=quay.io/kuberay/operator:nightly"},
				{Run: "kubectl apply -f https://raw.githubusercontent.com/ray-project/kuberay/$KUBERAY_VERSION/x.yaml"},
				{Run: "kubectl create -k \"github.com/ray-project/kuberay/ray-operator/config/default?ref=$KUBERAY_VERSION\""},
				{Run: "helm install prometheus prometheus-community/kube-prometheus-stack --version 45.0.0"},
				{Run: "curl -LO https://github.com/ray-project/kuberay/releases/download/v1.7.0/kubectl-ray_v1.7.0_linux_amd64.tar.gz"},
				{Run: "curl -LO https://github.com/ray-project/kuberay/releases/download/$KUBERAY_VERSION/kubectl-ray_${KUBERAY_VERSION}_linux_amd64.tar.gz"},
			}}},
			want: []string{"kuberay/master/", "kuberay/v1.7.0/", "ref=v1.5.1", "--version 1.5.1", "operator:nightly", "releases/download/v1.7.0"},
		},
		{
			name: "covered_by must name a unit with steps",
			units: []Unit{
				{ID: "doc", SourceSHA: "a", CoveredBy: "sample"},
				{ID: "sample", SourceSHA: "b", CoveredBy: "doc"},
				{ID: "gpu", SourceSHA: "c", CoveredBy: "gpu"},
			},
			want: []string{
				"doc: covered_by sample, which has no steps of its own", "sample: covered_by doc, which has no steps of its own",
				"gpu: the Item has requires", "gpu: covered_by itself",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			problems := lintManifest(items, tc.units)
			require.Len(t, problems, len(tc.want), strings.Join(problems, "\n"))
			for _, w := range tc.want {
				assert.True(t, containsAny(problems, w), "no problem mentions %q in:\n%s", w, strings.Join(problems, "\n"))
			}
		})
	}
}

func containsAny(problems []string, s string) bool {
	for _, p := range problems {
		if strings.Contains(p, s) {
			return true
		}
	}
	return false
}

func TestShell(t *testing.T) {
	u := &unitRun{work: t.TempDir()}
	u.out = filepath.Join(u.work, ".out")
	log, err := os.Create(filepath.Join(u.work, "log"))
	require.NoError(t, err)
	defer log.Close()
	ctx := context.Background()

	code, timedOut := u.shell(ctx, 10*time.Second, log, "echo hi; true")
	assert.Equal(t, 0, code)
	assert.False(t, timedOut)

	code, timedOut = u.shell(ctx, 10*time.Second, log, "false\necho not reached")
	assert.Equal(t, 1, code, "bash -e stops at the first failing command")
	assert.False(t, timedOut)

	t.Setenv("ANTHROPIC_API_KEY", "secret")
	code, _ = u.shell(ctx, 10*time.Second, log, `test -z "$ANTHROPIC_API_KEY" && test -n "$OUT"`)
	assert.Equal(t, 0, code, "steps run without the credentials only triage needs")

	start := time.Now()
	_, timedOut = u.shell(ctx, time.Second, log, "sleep 30")
	assert.True(t, timedOut)
	assert.Less(t, time.Since(start), 8*time.Second)

	// A background process outlives its step and dies with the unit.
	code, _ = u.shell(ctx, 10*time.Second, log, "sleep 300 >/dev/null 2>&1 & echo $! > bg.pid")
	require.Equal(t, 0, code)
	pid := readPID(t, filepath.Join(u.work, "bg.pid"))
	assert.False(t, processGone(pid, 0))
	u.killGroups()
	assert.True(t, processGone(pid, 5*time.Second))
}

func TestRunUnit(t *testing.T) {
	fake := t.TempDir()
	state := filepath.Join(fake, "state")
	writeExecutable(t, filepath.Join(fake, "kubectl"), `#!/bin/sh
case "$1 $2" in
"get namespaces,clusterroles,clusterrolebindings,customresourcedefinitions,mutatingwebhookconfigurations,validatingwebhookconfigurations")
  [ -e "$FAKE_STATE.fail" ] && exit 1
  cat "$FAKE_STATE" 2>/dev/null ;;
"delete --timeout=120s")
  shift 2; for o in "$@"; do echo "$o" >> "$FAKE_STATE.deleted"; done ;;
esac
exit 0
`)
	writeExecutable(t, filepath.Join(fake, "helm"), "#!/bin/sh\n[ \"$1\" = list ] && echo '[]'\nexit 0\n")
	t.Setenv("PATH", fake+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_STATE", state)
	t.Setenv("KUBERAY_VERSION", "v9.9.9")
	t.Setenv("CHART_VERSION", "9.9.9")

	results := t.TempDir()
	r := &runner{p: paths{results: results}, version: "v9.9.9", envDesc: "fake"}
	ctx := context.Background()

	t.Run("pass", func(t *testing.T) {
		unit := Unit{ID: "ok", SourceSHA: "0123456789abcdef", Steps: []Step{
			{Name: "Say the versions", Run: "echo hello $KUBERAY_VERSION $CHART_VERSION", Check: `grep -q "hello v9.9.9 9.9.9" "$OUT"`},
		}}
		failed, err := r.runUnit(ctx, Item{ID: "ok", Source: "x.yaml"}, unit)
		require.NoError(t, err)
		assert.Equal(t, 0, failed)
		fm, err := readFrontMatter(filepath.Join(results, "ok", "report.md"))
		require.NoError(t, err)
		assert.Equal(t, "pass", fm.Status)
		assert.Nil(t, fm.FailedStep)
		assert.True(t, done(filepath.Join(results, "ok", "report.md")))
		assert.NoDirExists(t, filepath.Join(results, "ok", "evidence"))
		assert.NoFileExists(t, filepath.Join(results, "ok", "swept.txt"))
	})

	t.Run("fail", func(t *testing.T) {
		unit := Unit{ID: "broken", SourceSHA: "0123456789abcdef", Notes: []string{"doc-bug: something"}, Steps: []Step{
			{Run: "echo namespace/leftover >> \"$FAKE_STATE\"\nsleep 300 >/dev/null 2>&1 &\necho $! > bg.pid", Check: "test -s bg.pid"},
			{Name: "Fails", Run: "cp bg.pid \"$FAKE_STATE.pid\"\nfalse"},
			{Name: "Never reached", Run: "true"},
		}, Cleanup: []string{"echo cleaned"}}
		failed, err := r.runUnit(ctx, Item{ID: "broken", Procedure: "p"}, unit)
		require.NoError(t, err)
		assert.Equal(t, 2, failed)
		dir := filepath.Join(results, "broken")
		fm, err := readFrontMatter(filepath.Join(dir, "report.md"))
		require.NoError(t, err)
		assert.Equal(t, "fail", fm.Status)
		require.NotNil(t, fm.FailedStep)
		assert.Equal(t, 2, *fm.FailedStep)
		assert.Equal(t, "step 2 run exit 1", fm.Brief)

		report := readFile(t, filepath.Join(dir, "report.md"))
		assert.Contains(t, report, "| 3 | Never reached | ⏭ not run | | |")
		assert.Contains(t, report, "- doc-bug: something")
		assert.Contains(t, report, "- namespace/leftover")
		assert.Contains(t, readFile(t, filepath.Join(dir, "steps", "01.log")), "# check exit 0")
		assert.Contains(t, readFile(t, filepath.Join(dir, "steps", "cleanup.log")), "cleaned")
		assert.FileExists(t, filepath.Join(dir, "evidence", "pods.txt"))
		assert.Equal(t, "namespace/leftover\n", readFile(t, state+".deleted"))
		assert.True(t, processGone(readPID(t, state+".pid"), 5*time.Second), "the port-forward-like process survived the unit")
		assert.Contains(t, report, "[log](steps/02.log), 6 lines |")
	})

	t.Run("snapshot fails", func(t *testing.T) {
		require.NoError(t, os.WriteFile(state+".fail", nil, 0o600))
		defer os.Remove(state + ".fail")
		unit := Unit{ID: "blind", SourceSHA: "0123456789abcdef", Steps: []Step{{Run: "echo namespace/orphan >> \"$FAKE_STATE\""}}}
		_, err := r.runUnit(ctx, Item{ID: "blind", Procedure: "p"}, unit)
		require.NoError(t, err)
		dir := filepath.Join(results, "blind")
		assert.Contains(t, readFile(t, filepath.Join(dir, "report.md")), "## Sweep skipped\n\nThe cluster could not be listed before the unit (kubectl get: exit status 1)")
		assert.Contains(t, readFile(t, filepath.Join(dir, "steps", "cleanup.log")), "sweep skipped: kubectl get")
		assert.Equal(t, "namespace/leftover\n", readFile(t, state+".deleted"), "nothing may be deleted when the baseline is unknown")
	})

	t.Run("interrupted", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		time.AfterFunc(300*time.Millisecond, cancel)
		unit := Unit{ID: "stopped", SourceSHA: "0123456789abcdef", Steps: []Step{
			{Run: "sleep 30"},
			{Name: "Never reached", Run: "true"},
		}, Cleanup: []string{"echo cleaned after interrupt"}}
		_, err := r.runUnit(cctx, Item{ID: "stopped", Procedure: "p"}, unit)
		require.ErrorIs(t, err, errInterrupted)
		dir := filepath.Join(results, "stopped")
		assert.NoFileExists(t, filepath.Join(dir, "report.md"), "an interrupted unit must run again next time")
		assert.Contains(t, readFile(t, filepath.Join(dir, "steps", "cleanup.log")), "cleaned after interrupt", "cleanup runs even though ctx is canceled")
		assert.NoDirExists(t, filepath.Join(dir, "evidence"))
		assert.False(t, done(filepath.Join(dir, "report.md")))
	})
}

func TestFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			fmt.Fprint(w, "body")
		case "/gone":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusTooManyRequests)
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	body, _, found, err := fetch(ctx, srv.URL+"/ok")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "body", string(body))

	_, _, found, err = fetch(ctx, srv.URL+"/gone")
	require.NoError(t, err)
	assert.False(t, found, "404 is the only status that means a removed source")

	_, _, _, err = fetch(ctx, srv.URL+"/limited")
	require.ErrorContains(t, err, "HTTP 429", "a rate limit must not be taken for a removed source")
}

func TestChartPublished(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "apiVersion: v1\nentries:\n  kuberay-operator:\n  - version: 1.8.0-rc.0\n  - version: 1.7.0\n  ray-cluster:\n  - version: 1.7.0\n")
	}))
	defer srv.Close()
	old := chartIndex
	chartIndex = srv.URL + "/index.yaml"
	defer func() { chartIndex = old }()

	require.NoError(t, chartPublished(context.Background(), "1.8.0-rc.0"))
	err := chartPublished(context.Background(), "1.8.0")
	require.ErrorContains(t, err, "chart kuberay-operator 1.8.0 is not in")
}

func TestValidTag(t *testing.T) {
	for _, ok := range []string{"v1.8.0", "v1.8.0-rc.0", "v10.0.12-rc.3"} {
		assert.True(t, validTag.MatchString(ok), ok)
	}
	for _, bad := range []string{"1.8.0", "v1.8", "master", "../../..", "v1.8.0-rc", "v1.8.0/x"} {
		assert.False(t, validTag.MatchString(bad), bad)
	}
}

func TestRestoreFrontMatter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.md")
	two := 2
	original := front(frontMatter{Item: "x", Status: "fail", Version: "v9.9.9", FailedStep: &two, Brief: "step 2 run exit 1"}) + "# x\n\n| table |\n"

	// Unquoted brief with a colon: YAML cannot parse it, the summary would say Not run.
	broken := "---\nitem: x\nstatus: fail\nversion: v9.9.9\nfailed_step: 2\ntriage: doc-bug\nbrief: page says: use -n ray-system\n---\n\n# x\n\n| table |\n\n## Triage\n\nThe cause.\n"
	require.NoError(t, os.WriteFile(path, []byte(broken), 0o600))
	_, err := readFrontMatter(path)
	require.Error(t, err)
	assert.True(t, done(path), "a report that exists but does not parse is kept")

	require.NoError(t, restoreFrontMatter(path, []byte(original)))
	fm, err := readFrontMatter(path)
	require.NoError(t, err)
	assert.Equal(t, "fail", fm.Status)
	assert.Equal(t, 2, *fm.FailedStep)
	assert.Equal(t, "doc-bug", fm.Triage)
	assert.Equal(t, "page says: use -n ray-system", fm.Brief)
	assert.Contains(t, readFile(t, path), "\n---\n\n# x\n\n| table |\n\n## Triage\n\nThe cause.\n", "the body survives")

	// No front matter at all: everything becomes the body.
	require.NoError(t, os.WriteFile(path, []byte("# x\n\n## Triage\n\nflaky\n"), 0o600))
	require.NoError(t, restoreFrontMatter(path, []byte(original)))
	fm, err = readFrontMatter(path)
	require.NoError(t, err)
	assert.Empty(t, fm.Triage)
	assert.Equal(t, "step 2 run exit 1", fm.Brief)
	assert.Contains(t, readFile(t, path), "\n---\n\n# x\n\n## Triage\n\nflaky\n")
}

func TestFresh(t *testing.T) {
	results := t.TempDir()
	r := &runner{p: paths{results: results}, version: "v9.9.9"}
	items := map[string]Item{
		"same":    {ID: "same", Procedure: "do this"},
		"changed": {ID: "changed", Procedure: "do that"},
	}
	units := []Unit{
		{ID: "same", SourceSHA: blobSHA([]byte("do this"))},
		{ID: "changed", SourceSHA: "0000000000000000000000000000000000000000"},
	}
	fresh, err := r.fresh(context.Background(), items, units)
	require.NoError(t, err)
	require.Len(t, fresh, 1)
	assert.Equal(t, "same", fresh[0].ID)

	report := filepath.Join(results, "changed", "report.md")
	fm, err := readFrontMatter(report)
	require.NoError(t, err)
	assert.Equal(t, "stale", fm.Status)
	assert.Equal(t, "v9.9.9", fm.Version)
	assert.Contains(t, fm.Brief, "source changed since the unit was planned (000000000000 is now ")
	assert.False(t, done(report), "a stale report is not a result: the unit may be re-planned")
	assert.False(t, done(filepath.Join(results, "same", "report.md")))
}

func TestRenderSummary(t *testing.T) {
	results := t.TempDir()
	writeReport := func(id, frontMatter string) {
		require.NoError(t, os.MkdirAll(filepath.Join(results, id), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(results, id, "report.md"), []byte("---\n"+frontMatter+"---\n\n# "+id+"\n"), 0o600))
	}
	writeReport("doc", "item: doc\nstatus: pass\nversion: v9.9.9\nfailed_step:\ntriage: \"\"\nbrief: \"\"\n")
	writeReport("broken", "item: broken\nstatus: fail\nversion: v9.9.9\nfailed_step: 4\ntriage: doc-bug\nbrief: \"doc says 2 workers | sample has 1\"\n")
	require.NoError(t, os.WriteFile(filepath.Join(results, "broken", "triage.json"), []byte(`{"total_cost_usd": 0.16}`), 0o600))
	writeReport("old", "item: old\nstatus: stale\nversion: v9.9.9\nfailed_step:\ntriage: \"\"\nbrief: \"source changed since the unit was planned (aaaaaaaaaaaa is now bbbbbbbbbbbb)\"\n")
	writeReport("mangled", "item: mangled\nstatus: fail\nbrief: page says: no\n")

	items := []Item{
		{ID: "doc"},
		{ID: "sample"},
		{ID: "broken"},
		{ID: "old"},
		{ID: "mangled"},
		{ID: "planned"},
		{ID: "unplanned"},
		{ID: "gpu", Source: "g.yaml", Requires: []string{"gpu", "gke"}},
	}
	units := []Unit{{ID: "doc"}, {ID: "sample", CoveredBy: "doc"}, {ID: "broken"}, {ID: "old"}, {ID: "mangled"}, {ID: "planned"}}
	summary, counts := renderSummary(results, items, units, "", time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC))

	assert.Equal(t, "2 pass, 1 fail, 1 stale, 1 manual, 1 not run, 1 not planned, 1 unreadable", counts)
	for _, want := range []string{
		"KubeRay v9.9.9, 2026-10-10. " + counts + ". Triage cost $0.16.",
		`| [broken](broken/report.md) | 4 | doc-bug | doc says 2 workers \| sample has 1 |`,
		"| gpu | gpu, gke | g.yaml |",
		"## Stale\n\nNot run: the page or sample changed",
		"| [old](old/report.md) | source changed since the unit was planned (aaaaaaaaaaaa is now bbbbbbbbbbbb) |",
		"## Unreadable\n\nA report.md exists but its front matter does not parse.",
		"| [mangled](mangled/report.md) | ",
		"## Not run\n\nPlanned, but no report",
		"- planned\n",
		"- unplanned\n",
		"- [sample](doc/report.md) (via doc)\n",
	} {
		assert.Contains(t, summary, want)
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	require.NoError(t, os.Chmod(path, 0o700))
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(path))
	require.NoError(t, err)
	return string(data)
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	pid, err := strconv.Atoi(strings.TrimSpace(readFile(t, path)))
	require.NoError(t, err)
	return pid
}

// processGone polls until pid is gone; an unreaped zombie counts as gone.
func processGone(pid int, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Kill(pid, 0)
		stat, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if errors.Is(err, syscall.ESRCH) || strings.Contains(string(stat), ") Z ") {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}
