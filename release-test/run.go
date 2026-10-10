package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"sigs.k8s.io/yaml"
)

var (
	chartIndex     = "https://ray-project.github.io/kuberay-helm/index.yaml"
	errInterrupted = errors.New("interrupted; the unit has no report and runs again next time")
	// triageClass and triageBrief salvage a front matter the triage skill wrote badly.
	triageClass = regexp.MustCompile(`(?m)^triage:\s*"?(plan-bug|doc-bug|sample-bug|product-bug|env|flaky)"?\s*$`)
	triageBrief = regexp.MustCompile(`(?m)^brief:\s*(.+?)\s*$`)
)

// runner executes units on the current kube context. A pass costs no tokens; a failure gets
// evidence, cleanup and one headless Claude call.
type runner struct {
	p       paths
	version string
	cluster string // only units whose cluster equals this run
	triage  bool
	model   string
	envDesc string // the cluster, as the reports describe it
}

func cmdRun(ctx context.Context, p paths, ids []string) error {
	items, err := loadItems(p.items)
	if err != nil {
		return err
	}
	units, err := loadManifest(p.manifest)
	if err != nil {
		return err
	}
	if problems := lintManifest(items, units); len(problems) > 0 {
		printProblems(problems)
		return errLint
	}
	r := runner{
		p:       p,
		version: os.Getenv("KUBERAY_VERSION"),
		cluster: cmp.Or(os.Getenv("CLUSTER"), "default"),
		triage:  os.Getenv("TRIAGE") != "0",
		model:   cmp.Or(os.Getenv("TRIAGE_MODEL"), "sonnet"),
	}
	if r.version == "" {
		return errors.New("set KUBERAY_VERSION to the tag under test, e.g. v1.8.0-rc.0")
	}
	// Steps inherit both.
	os.Setenv("CHART_VERSION", strings.TrimPrefix(r.version, "v"))
	if err := r.preflight(ctx); err != nil {
		return err
	}
	if err := chartPublished(ctx, os.Getenv("CHART_VERSION")); err != nil {
		return err
	}

	byID, planned := itemsByID(items), unitsByID(units)
	rerun := len(ids) > 0
	if !rerun {
		for _, it := range items {
			ids = append(ids, it.ID)
		}
	}
	if err := os.MkdirAll(p.results, 0o750); err != nil {
		return err
	}
	var todo []Unit
	for _, id := range ids {
		it, ok := byID[id]
		if !ok {
			return fmt.Errorf("no Item %s in items.yaml", id)
		}
		u, ok := planned[id]
		var skip string
		switch {
		case len(it.Requires) > 0:
			skip = "requires " + strings.Join(it.Requires, ",")
		case !ok:
			skip = "not planned"
		case u.CoveredBy != "":
			skip = "covered by " + u.CoveredBy
		case cmp.Or(u.Cluster, "default") != r.cluster:
			skip = "needs CLUSTER=" + u.Cluster
		}
		if skip != "" {
			if rerun {
				fmt.Printf("%s: skipped, %s\n", id, skip)
			}
			continue
		}
		if !rerun && done(filepath.Join(p.results, id, "report.md")) {
			continue
		}
		todo = append(todo, u)
	}
	todo, err = r.fresh(ctx, byID, todo)
	if err != nil {
		return err
	}
	streak := 0 // units in a row that failed at their first step
	for _, u := range todo {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fmt.Printf("%s ... ", u.ID)
		failed, err := r.runUnit(ctx, byID[u.ID], u)
		if err != nil {
			return fmt.Errorf("%s: %w", u.ID, err)
		}
		if failed != 1 {
			streak = 0
		} else if streak++; streak == 3 {
			return errors.New("3 units in a row failed at their first step: the environment is broken, not the release; see their reports, then run again")
		}
	}
	return cmdSummary(p)
}

// chartPublished: a tag whose charts are not out yet, or a typo, would fail every unit at its
// first step and triage each failure.
func chartPublished(ctx context.Context, chartVersion string) error {
	body, _, found, err := fetch(ctx, chartIndex)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%s not found", chartIndex)
	}
	var index struct {
		Entries map[string][]struct {
			Version string `json:"version"`
		} `json:"entries"`
	}
	if err := yaml.Unmarshal(body, &index); err != nil {
		return fmt.Errorf("chart index: %w", err)
	}
	for _, e := range index.Entries["kuberay-operator"] {
		if e.Version == chartVersion {
			return nil
		}
	}
	return fmt.Errorf("chart kuberay-operator %s is not in %s; release.md step 6 publishes the charts", chartVersion, chartIndex)
}

// done: the unit has a result for this version. A stale report is not one; a report that does
// not parse is, so a plain run never destroys a triage someone wrote badly.
func done(report string) bool {
	fm, err := readFrontMatter(report)
	if err != nil {
		_, err := os.Stat(filepath.Clean(report))
		return err == nil
	}
	return fm.Status != "stale"
}

// fresh drops units whose source changed or vanished since planning and writes each a stale
// report: steps written for an older page would test the wrong thing. Needs the network.
func (r *runner) fresh(ctx context.Context, byID map[string]Item, units []Unit) ([]Unit, error) {
	if len(units) > 0 {
		fmt.Printf("checking the sources of %d units\n", len(units))
	}
	var fresh []Unit
	for _, u := range units {
		src, err := r.p.fetchSource(ctx, byID[u.ID])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", u.ID, err)
		}
		if src.dead == "" && src.sha == u.SourceSHA {
			fresh = append(fresh, u)
			continue
		}
		brief := fmt.Sprintf("source changed since the unit was planned (%.12s is now %.12s)", u.SourceSHA, src.sha)
		if src.dead != "" {
			brief = "source is gone: " + src.dead
		}
		dir := filepath.Join(r.p.results, u.ID)
		if err := os.RemoveAll(dir); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, err
		}
		report := front(frontMatter{Item: u.ID, Status: "stale", Version: r.version, Brief: brief}) + fmt.Sprintf(
			"# %s\n\nNot run: %s. Re-plan it with `/release-test-plan %s`, review the manifest diff, then run `go run ./release-test run %s`.\n",
			u.ID, brief, u.ID, u.ID)
		if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte(report), 0o600); err != nil {
			return nil, err
		}
		fmt.Printf("%s: stale, %s\n", u.ID, brief)
	}
	return fresh, nil
}

func (r *runner) preflight(ctx context.Context) error {
	kctx, err := output(ctx, "kubectl", "config", "current-context")
	if err != nil {
		return errors.New("no current kube context")
	}
	if !strings.HasPrefix(kctx, "kind-") && os.Getenv("ANY_CONTEXT") != "1" {
		return fmt.Errorf("context %s is not a kind cluster; the runner deletes what units leave behind (ANY_CONTEXT=1 to override)", kctx)
	}
	if _, err := output(ctx, "kubectl", "get", "--raw", "/readyz"); err != nil {
		return fmt.Errorf("cluster %s is not reachable", kctx)
	}
	var version struct {
		ServerVersion struct {
			GitVersion string `json:"gitVersion"`
		} `json:"serverVersion"`
	}
	out, _ := output(ctx, "kubectl", "version", "-o", "json")
	_ = json.Unmarshal([]byte(out), &version)
	node, _ := output(ctx, "kubectl", "get", "nodes", "-o",
		"jsonpath={.items[0].status.allocatable.cpu} cpu / {.items[0].status.allocatable.memory}")
	r.envDesc = fmt.Sprintf("%s, Kubernetes %s, node %s", kctx, version.ServerVersion.GitVersion, node)
	return nil
}

// unitRun is the state of one unit while it runs.
type unitRun struct {
	work   string // the steps' working directory
	out    string // holds the current step's run output; steps see it as $OUT
	groups []int  // process groups started, killed when the unit ends
}

// runUnit runs one unit and returns the step it failed at, 0 for a pass. When ctx is canceled
// (Ctrl-C, a CI timeout) it still runs the cleanup, under its own deadline, and returns
// errInterrupted without a report, so the next run repeats the unit.
func (r *runner) runUnit(ctx context.Context, it Item, unit Unit) (failed int, err error) {
	dir := filepath.Join(r.p.results, unit.ID)
	if err := os.RemoveAll(dir); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "steps"), 0o750); err != nil {
		return 0, err
	}
	work, err := os.MkdirTemp("", "release-test-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(work)
	u := &unitRun{work: work, out: filepath.Join(work, ".out")}
	defer u.killGroups()
	before, snapErr := r.snapshot(ctx)
	start := time.Now()
	started := start.UTC().Format(time.RFC3339)

	var rows []string
	brief := ""
	for i, step := range unit.Steps {
		num := fmt.Sprintf("%02d", i+1)
		logPath := filepath.Join(dir, "steps", num+".log")
		result, took, err := u.step(ctx, logPath, step)
		if err != nil {
			return 0, err
		}
		rows = append(rows, fmt.Sprintf("| %d | %s | %s | %ds | [log](steps/%s.log), %d lines |",
			i+1, cell(cmp.Or(step.Name, step.Run)), result, took, num, countLines(logPath)))
		if result != "✅" {
			failed = i + 1
			brief = fmt.Sprintf("step %d %s", failed, strings.TrimPrefix(result, "❌ "))
			break
		}
	}
	interrupted := ctx.Err() != nil
	if failed > 0 && !interrupted {
		for i := failed; i < len(unit.Steps); i++ {
			rows = append(rows, fmt.Sprintf("| %d | %s | ⏭ not run | | |", i+1, cell(cmp.Or(unit.Steps[i].Name, unit.Steps[i].Run))))
		}
		r.evidence(ctx, filepath.Join(dir, "evidence"))
	}

	cctx := ctx
	if interrupted {
		// Cleanup gets a fresh context with a deadline; a second Ctrl-C stops it.
		var stop, cancel context.CancelFunc
		cctx, stop = signal.NotifyContext(context.WithoutCancel(ctx), os.Interrupt, syscall.SIGTERM)
		defer stop()
		cctx, cancel = context.WithTimeout(cctx, 10*time.Minute)
		defer cancel()
	}
	cleanupLog, err := os.OpenFile(filepath.Join(dir, "steps", "cleanup.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer cleanupLog.Close()
	for _, c := range unit.Cleanup {
		fmt.Fprint(cleanupLog, prefixLines(c, "❯ ", "  "))
		u.shell(cctx, defaultTimeout*time.Second, cleanupLog, c)
	}
	u.killGroups()
	var swept []string
	if snapErr == nil {
		swept = r.sweep(cctx, before, cleanupLog)
	} else {
		fmt.Fprintf(cleanupLog, "\nsweep skipped: %v\n", snapErr)
	}
	if len(swept) > 0 {
		if err := os.WriteFile(filepath.Join(dir, "swept.txt"), []byte(strings.Join(swept, "\n")+"\n"), 0o600); err != nil {
			return 0, err
		}
	}
	if interrupted {
		fmt.Println("interrupted; cleanup done")
		return 0, errInterrupted
	}

	status := "pass"
	if failed > 0 {
		status = "fail"
	}
	took := int(time.Since(start).Seconds())
	report := r.report(it, unit, status, failed, brief, started, took, rows, swept, snapErr)
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte(report), 0o600); err != nil {
		return 0, err
	}
	if failed > 0 {
		fmt.Printf("fail at step %d (%ds)\n", failed, took)
	} else {
		fmt.Printf("pass (%ds)\n", took)
	}
	if failed > 0 && r.triage {
		r.triageUnit(ctx, it, dir)
	}
	return failed, nil
}

// countLines of a log, so the triage skill can read its tail without reading it all.
func countLines(path string) int {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "\n")
}

// step runs one step and logs it; returns "✅" or the failure, and the seconds it took.
func (u *unitRun) step(ctx context.Context, logPath string, step Step) (string, int, error) {
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", 0, err
	}
	defer log.Close()
	if step.Name != "" {
		fmt.Fprintf(log, "# %s\n", step.Name)
	}
	fmt.Fprint(log, prefixLines(step.Run, "❯ ", "  "))
	if step.Check != "" {
		fmt.Fprint(log, prefixLines(step.Check, "# check: ", "#        "))
	}
	fmt.Fprintln(log)

	timeout := time.Duration(cmp.Or(step.Timeout, defaultTimeout)) * time.Second
	start := time.Now()
	out, err := os.Create(u.out)
	if err != nil {
		return "", 0, err
	}
	code, timedOut := u.shell(ctx, timeout, out, step.Run)
	out.Close()
	if data, err := os.ReadFile(u.out); err == nil {
		_, _ = log.Write(data)
	}
	what := "run"
	if code == 0 && !timedOut && step.Check != "" {
		fmt.Fprint(log, "\n❯ check\n")
		code, timedOut = u.shell(ctx, timeout, log, step.Check)
		what = "check"
	}
	took := int(time.Since(start).Seconds())
	fmt.Fprintf(log, "\n# %s exit %d after %ds\n", what, code, took)
	switch {
	case timedOut:
		return fmt.Sprintf("❌ %s timed out after %ds", what, int(timeout.Seconds())), took, nil
	case code != 0:
		return fmt.Sprintf("❌ %s exit %d", what, code), took, nil
	}
	return "✅", took, nil
}

// shell runs a script with bash -e in its own process group, so a timeout kills everything it
// started and leftovers (port-forwards) die in killGroups. out must be a file: a pipe would make
// Wait block on those leftovers.
func (u *unitRun) shell(ctx context.Context, timeout time.Duration, out *os.File, script string) (code int, timedOut bool) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-ec", script)
	cmd.Dir = u.work
	cmd.Env = append(stepEnv(), "OUT="+u.out)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(out, err)
		return -1, false
	}
	u.groups = append(u.groups, cmd.Process.Pid)
	_ = cmd.Wait()
	return cmd.ProcessState.ExitCode(), errors.Is(ctx.Err(), context.DeadlineExceeded)
}

// stepEnv: our environment without the credentials only triage needs; step output is kept.
func stepEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "ANTHROPIC_") || strings.HasPrefix(kv, "CLAUDE_CODE_") {
			continue
		}
		env = append(env, kv)
	}
	return env
}

func (u *unitRun) killGroups() {
	for _, pgid := range u.groups {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
	u.groups = nil
}

// snapshot lists what a unit could leave behind. An error means the list is unknown, and sweep
// must not run on it: an empty list would make everything in the cluster look new.
func (r *runner) snapshot(ctx context.Context) ([]string, error) {
	out, err := output(ctx, "kubectl", "get",
		"namespaces,clusterroles,clusterrolebindings,customresourcedefinitions,mutatingwebhookconfigurations,validatingwebhookconfigurations",
		"-o", "name")
	if err != nil {
		return nil, fmt.Errorf("kubectl get: %w", err)
	}
	objs := lines(out)
	// Ray resources can be listed only while their CRDs are installed.
	var kinds []string
	for _, o := range objs {
		if name, ok := strings.CutPrefix(o, "customresourcedefinition.apiextensions.k8s.io/"); ok && strings.HasSuffix(name, ".ray.io") {
			kinds = append(kinds, name)
		}
	}
	if len(kinds) > 0 {
		out, err = output(ctx, "kubectl", "get", strings.Join(kinds, ","), "-A",
			"-o", `jsonpath={range .items[*]}{.kind}/{.metadata.namespace}/{.metadata.name}{"\n"}{end}`)
		if err != nil {
			return nil, fmt.Errorf("kubectl get ray resources: %w", err)
		}
		objs = append(objs, lines(out)...)
	}
	out, err = output(ctx, "helm", "list", "-A", "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("helm list: %w", err)
	}
	var releases []struct{ Name, Namespace string }
	if err := json.Unmarshal([]byte(out), &releases); err != nil {
		return nil, fmt.Errorf("helm list: %w", err)
	}
	for _, rel := range releases {
		objs = append(objs, "helm/"+rel.Namespace+"/"+rel.Name)
	}
	return objs, nil
}

// sweep deletes what the unit created and its cleanup left, and returns it for the report.
func (r *runner) sweep(ctx context.Context, before []string, log *os.File) []string {
	after, err := r.snapshot(ctx)
	if err != nil {
		fmt.Fprintf(log, "\nsweep skipped: %v\n", err)
		return nil
	}
	var left []string
	for _, o := range after {
		if !slices.Contains(before, o) {
			left = append(left, o)
		}
	}
	slices.Sort(left)
	var rest []string
	// Ray resources first, while the operator that removes their finalizers still runs.
	for _, o := range left {
		if kind, ns, name, ok := splitRayObject(o); ok {
			toFile(ctx, log, "kubectl", "delete", strings.ToLower(kind)+".ray.io", name, "-n", ns, "--timeout=60s")
		}
	}
	for _, o := range left {
		if rel, ok := strings.CutPrefix(o, "helm/"); ok {
			ns, name, _ := strings.Cut(rel, "/")
			toFile(ctx, log, "helm", "uninstall", name, "-n", ns, "--wait", "--timeout", "2m")
		} else if _, _, _, ray := splitRayObject(o); !ray {
			rest = append(rest, o)
		}
	}
	if len(rest) > 0 {
		// Wait, so the next unit can create a namespace of the same name.
		toFile(ctx, log, "kubectl", append([]string{"delete", "--timeout=120s"}, rest...)...)
	}
	return left
}

// splitRayObject splits "RayCluster/<namespace>/<name>".
func splitRayObject(o string) (kind, ns, name string, ok bool) {
	parts := strings.Split(o, "/")
	if len(parts) != 3 || !strings.HasPrefix(parts[0], "Ray") {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

func (r *runner) evidence(ctx context.Context, dir string) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return
	}
	save := func(name string, args ...string) {
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		toFile(ctx, f, "kubectl", args...)
	}
	save("pods.txt", "get", "pods", "-A", "-o", "wide")
	save("events.txt", "get", "events", "-A", "--sort-by=.lastTimestamp")
	save("ray-resources.yaml", "get", "rayclusters,rayjobs,rayservices,raycronjobs", "-A", "-o", "yaml")
	namespaces, _ := output(ctx, "kubectl", "get", "namespaces", "-o", "jsonpath={.items[*].metadata.name}")
	for ns := range strings.FieldsSeq(namespaces) {
		if strings.HasPrefix(ns, "kube-") || ns == "local-path-storage" {
			continue
		}
		save("describe-pods.txt", "describe", "pods", "-n", ns)
		pods, _ := output(ctx, "kubectl", "get", "pods", "-n", ns, "-o", "name")
		for _, pod := range lines(pods) {
			save("pod-logs.txt", "logs", "-n", ns, pod, "--all-containers", "--prefix", "--tail=500")
		}
	}
}

func (r *runner) report(it Item, unit Unit, status string, failed int, brief, started string, took int, rows, swept []string, sweepErr error) string {
	var b strings.Builder
	fm := frontMatter{Item: unit.ID, Status: status, Version: r.version, Brief: brief}
	if failed > 0 {
		fm.FailedStep = &failed
	}
	b.WriteString(front(fm))
	src := cmp.Or(it.Source, "procedure in items.yaml")
	fmt.Fprintf(&b, "# %s\n\n| | |\n|---|---|\n", unit.ID)
	fmt.Fprintf(&b, "| Source | %s (`%.12s`) |\n| KubeRay | %s |\n| Cluster | %s |\n| Ran | %s, %ds |\n\n",
		src, unit.SourceSHA, r.version, r.envDesc, started, took)
	b.WriteString("## Steps\n\n| # | Step | Result | Time | Log |\n|---|---|---|---|---|\n")
	b.WriteString(strings.Join(rows, "\n"))
	b.WriteString("\n")
	if len(unit.Notes) > 0 {
		b.WriteString("\n## Plan notes\n\n")
		for _, n := range unit.Notes {
			b.WriteString("- ")
			b.WriteString(n)
			b.WriteString("\n")
		}
	}
	if len(swept) > 0 {
		b.WriteString("\n## Left behind by cleanup\n\nThe runner deleted these after the unit's cleanup:\n\n")
		for _, o := range swept {
			b.WriteString("- ")
			b.WriteString(o)
			b.WriteString("\n")
		}
	}
	if sweepErr != nil {
		fmt.Fprintf(&b, "\n## Sweep skipped\n\nThe cluster could not be listed before the unit (%v), so nothing was deleted after it. "+
			"What this unit left behind is now part of the next unit's baseline; check the cluster by hand.\n", sweepErr)
	}
	return b.String()
}

// front renders a report's front matter; triage fills in triage and brief.
func front(fm frontMatter) string {
	failedStep := ""
	if fm.FailedStep != nil {
		failedStep = strconv.Itoa(*fm.FailedStep)
	}
	return fmt.Sprintf("---\nitem: %s\nstatus: %s\nversion: %s\nfailed_step: %s\ntriage: %q\nbrief: %q\n---\n\n",
		fm.Item, fm.Status, fm.Version, failedStep, fm.Triage, fm.Brief)
}

// triageUnit asks the release-test-run skill why the unit failed. The skill reads Pod logs
// nobody vetted, so it gets no shell and no network, reads only this repository and the results
// directory, and may edit only the unit's report.md. The page is prefetched into the cache.
func (r *runner) triageUnit(ctx context.Context, it Item, dir string) {
	_, _ = r.p.fetchSource(ctx, it)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	dir, err := filepath.Abs(dir)
	if err != nil {
		fmt.Println("  triage skipped:", err)
		return
	}
	report := filepath.Join(dir, "report.md")
	original, err := os.ReadFile(report)
	if err != nil {
		fmt.Println("  triage skipped:", err)
		return
	}
	stdout, err1 := os.Create(filepath.Join(dir, "triage.json"))
	stderr, err2 := os.Create(filepath.Join(dir, "triage.err"))
	if err := errors.Join(err1, err2); err != nil {
		fmt.Println("  triage skipped:", err)
		return
	}
	defer stdout.Close()
	defer stderr.Close()
	cmd := exec.CommandContext(ctx, "claude", "-p", "/release-test-run "+dir, //nolint:gosec // fixed tool; dir is ours
		"--model", r.model, "--max-turns", "15", "--output-format", "json",
		"--permission-mode", "dontAsk",
		"--tools", "Read", "Grep", "Glob", "Edit",
		"--allowedTools", "Edit(//"+report+")", // one slash would anchor at the settings source
		"--add-dir", dir,
		"--settings", `{"permissions":{"blockReadsOutsideWorkingDirectories":true}}`)
	cmd.Dir = r.p.root
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		fmt.Printf("  triage failed, see %s\n", stderr.Name())
		return
	}
	fm, err := readFrontMatter(report)
	if err != nil {
		fmt.Printf("  triage left the front matter unreadable (%v); restoring it\n", err)
		if err := restoreFrontMatter(report, original); err != nil {
			fmt.Println(" ", err)
			return
		}
		fm, err = readFrontMatter(report)
	}
	if err == nil {
		fmt.Printf("  %s: %s\n", fm.Triage, fm.Brief)
	}
}

// restoreFrontMatter rewrites a report's front matter from the one the runner wrote, keeping the
// body the triage appended and salvaging the class and brief it tried to write.
func restoreFrontMatter(path string, original []byte) error {
	fm, err := parseFrontMatter(string(original))
	if err != nil {
		return fmt.Errorf("original front matter: %w", err)
	}
	broken, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return err
	}
	head, body := "", string(broken)
	if rest, ok := strings.CutPrefix(body, "---\n"); ok {
		if h, b, ok := strings.Cut(rest, "\n---\n"); ok {
			head, body = h, b
		}
	}
	if m := triageClass.FindStringSubmatch(head); m != nil {
		fm.Triage = m[1]
	}
	if m := triageBrief.FindStringSubmatch(head); m != nil {
		fm.Brief = strings.Trim(m[1], `"'`)
	}
	return os.WriteFile(path, []byte(front(fm)+strings.TrimLeft(body, "\n")), 0o600) //nolint:gosec // the report this run wrote, under a validated id
}

// output runs a command and returns its stdout; stderr is dropped (helm prints plugin warnings).
func output(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return strings.TrimSpace(string(out)), err
}

// toFile runs a command with stdout and stderr appended to f.
func toFile(ctx context.Context, f *os.File, name string, args ...string) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = f, f
	_ = cmd.Run()
}

// prefixLines prefixes the first line of s with first and the others with rest.
func prefixLines(s, first, rest string) string {
	var b strings.Builder
	for i, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if i == 0 {
			b.WriteString(first)
			b.WriteString(l)
			b.WriteString("\n")
		} else {
			b.WriteString(rest)
			b.WriteString(l)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// cell fits a command or step name into one markdown table cell.
func cell(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if r := []rune(s); len(r) > 70 {
		s = string(r[:70])
	}
	return strings.ReplaceAll(s, "|", `\|`)
}

func lines(s string) []string {
	var out []string
	for l := range strings.SplitSeq(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
