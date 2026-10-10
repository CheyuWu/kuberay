// Command release-test validates a KubeRay release candidate on a kind cluster. An LLM only plans
// units and triages failures; this program does the rest. See README.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

const usage = `usage: go run ./release-test <command> [ID...]

  stale [ID...]  list the Items whose manifest unit is missing or out of date, and fetch
                 their sources into release-test/.cache/ for the plan skill to read
  lint           check release-test/manifest.yaml
  run [ID...]    check that the tag's charts are published and the units' sources are still
                 what they were planned from, then run them on the current kind cluster;
                 with IDs only those, re-run even if they have a report
  summary        write release-test/results/<KUBERAY_VERSION>/SUMMARY.md

run reads KUBERAY_VERSION (required, the tag under test), CLUSTER (default "default"),
TRIAGE (0 skips the Claude call on failure), TRIAGE_MODEL (default "sonnet"), RESULTS
(default release-test/results/<KUBERAY_VERSION>) and ANY_CONTEXT (1 allows a non-kind
context). summary reads KUBERAY_VERSION or RESULTS. stale and run read RAY_DOCS_REF
(default "master").
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:])
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "release-test:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("no command")
	}
	p, err := findPaths(ctx)
	if err != nil {
		return err
	}
	switch args[0] {
	case "stale":
		return cmdStale(ctx, p, args[1:])
	case "lint":
		return cmdLint(p)
	case "run":
		return cmdRun(ctx, p, args[1:])
	case "summary":
		return cmdSummary(p)
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	}
	fmt.Fprint(os.Stderr, usage)
	return fmt.Errorf("unknown command %q", args[0])
}

// paths are the files the commands read and write.
type paths struct {
	root     string // the repository
	items    string
	manifest string
	cache    string // sources fetched by stale, read by the plan and triage skills
	results  string // this version's results; empty when neither RESULTS nor KUBERAY_VERSION is set
}

// validTag: KUBERAY_VERSION names the results directory and is spliced into URLs.
var validTag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$`)

func findPaths(ctx context.Context) (paths, error) {
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return paths{}, fmt.Errorf("run inside the kuberay repository: %w", err)
	}
	version := os.Getenv("KUBERAY_VERSION")
	if version != "" && !validTag.MatchString(version) {
		return paths{}, fmt.Errorf("KUBERAY_VERSION %q is not a release tag like v1.8.0-rc.0", version)
	}
	root := strings.TrimSpace(string(out))
	dir := filepath.Join(root, "release-test")
	// One results directory per version: a new candidate starts clean, the old one stays readable.
	results := os.Getenv("RESULTS")
	if results == "" && version != "" {
		results = filepath.Join(dir, "results", version)
	}
	return paths{
		root:     root,
		items:    filepath.Join(dir, "items.yaml"),
		manifest: filepath.Join(dir, "manifest.yaml"),
		cache:    filepath.Join(dir, ".cache"),
		results:  results,
	}, nil
}

// rel makes a path repository-relative, the way the skills refer to files.
func (p paths) rel(path string) string {
	if r, err := filepath.Rel(p.root, path); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	return path
}
