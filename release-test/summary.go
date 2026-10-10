package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// frontMatter heads results/<id>/report.md; the runner writes it, triage fills in triage and brief.
type frontMatter struct {
	Item       string `json:"item"`
	Status     string `json:"status"`
	Version    string `json:"version"`
	FailedStep *int   `json:"failed_step"`
	Triage     string `json:"triage"`
	Brief      string `json:"brief"`
}

func readFrontMatter(path string) (frontMatter, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return frontMatter{}, err
	}
	return parseFrontMatter(string(data))
}

func parseFrontMatter(report string) (frontMatter, error) {
	var fm frontMatter
	rest, ok := strings.CutPrefix(report, "---\n")
	if !ok {
		return fm, errors.New("no front matter")
	}
	head, _, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		return fm, errors.New("front matter is not closed")
	}
	err := yaml.Unmarshal([]byte(head), &fm)
	return fm, err
}

func cmdSummary(p paths) error {
	if p.results == "" {
		return errors.New("set KUBERAY_VERSION (or RESULTS) to pick the results directory")
	}
	items, err := loadItems(p.items)
	if err != nil {
		return err
	}
	units, err := loadManifest(p.manifest)
	if err != nil {
		return err
	}
	summary, counts := renderSummary(p.results, items, units, os.Getenv("KUBERAY_VERSION"), time.Now())
	if err := os.MkdirAll(p.results, 0o750); err != nil {
		return err
	}
	path := filepath.Join(p.results, "SUMMARY.md")
	if err := os.WriteFile(path, []byte(summary), 0o600); err != nil {
		return err
	}
	fmt.Printf("wrote %s: %s\n", p.rel(path), counts)
	return nil
}

// renderSummary has one row per Item; a covered Item gets its covering unit's verdict.
func renderSummary(results string, items []Item, units []Unit, version string, now time.Time) (summary, counts string) {
	planned := unitsByID(units)
	var fail, pass, stale, unreadable, manual, notRun, unplanned []string
	for _, it := range items {
		u, ok := planned[it.ID]
		rid := it.ID
		if u.CoveredBy != "" {
			rid = u.CoveredBy
		}
		link := fmt.Sprintf("[%s](%s/report.md)", it.ID, rid)
		if rid != it.ID {
			link += " (via " + rid + ")"
		}
		fm, err := readFrontMatter(filepath.Join(results, rid, "report.md"))
		switch {
		case err == nil:
			if version == "" {
				version = fm.Version
			}
			if fm.Status == "pass" {
				pass = append(pass, link)
				continue
			}
			if fm.Status == "stale" {
				stale = append(stale, fmt.Sprintf("| %s | %s |", link, strings.ReplaceAll(fm.Brief, "|", `\|`)))
				continue
			}
			step := ""
			if fm.FailedStep != nil {
				step = fmt.Sprint(*fm.FailedStep)
			}
			fail = append(fail, fmt.Sprintf("| %s | %s | %s | %s |", link, step, fm.Triage, strings.ReplaceAll(fm.Brief, "|", `\|`)))
		case !errors.Is(err, fs.ErrNotExist):
			unreadable = append(unreadable, fmt.Sprintf("| %s | %s |", link, strings.ReplaceAll(err.Error(), "|", `\|`)))
		case len(it.Requires) > 0:
			manual = append(manual, fmt.Sprintf("| %s | %s | %s |", it.ID, strings.Join(it.Requires, ", "), it.Source))
		case !ok:
			unplanned = append(unplanned, it.ID)
		default:
			notRun = append(notRun, it.ID)
		}
	}

	counts = fmt.Sprintf("%d pass, %d fail, %d stale, %d manual, %d not run, %d not planned",
		len(pass), len(fail), len(stale), len(manual), len(notRun), len(unplanned))
	if len(unreadable) > 0 {
		counts += fmt.Sprintf(", %d unreadable", len(unreadable))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Release test summary\n\nKubeRay %s, %s. %s. Triage cost $%.2f.\n",
		cmp.Or(version, "?"), now.UTC().Format("2006-01-02"), counts, triageCost(results))
	section := func(title, intro, header string, rows []string, bullet bool) {
		if len(rows) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n## %s\n\n", title)
		if intro != "" {
			b.WriteString(intro)
			b.WriteString("\n\n")
		}
		if header != "" {
			b.WriteString(header)
			b.WriteString("\n")
		}
		for _, r := range rows {
			if bullet {
				r = "- " + r
			}
			b.WriteString(r)
			b.WriteString("\n")
		}
	}
	section("Fail", "", "| Item | Step | Triage | Brief |\n|---|---|---|---|", fail, false)
	section("Unreadable", "A report.md exists but its front matter does not parse. Fix it by hand, or re-run the Item.",
		"| Item | Error |\n|---|---|", unreadable, false)
	section("Stale", "Not run: the page or sample changed since the unit was planned. "+
		"`/release-test-plan <id>` re-plans a unit; after the manifest is reviewed, `go run ./release-test run <id>` runs it.",
		"| Item | Why |\n|---|---|", stale, false)
	section("Manual", "Needs hardware or a cloud a kind cluster cannot provide; a human runs these.",
		"| Item | Requires | Source |\n|---|---|---|", manual, false)
	section("Not run", "Planned, but no report: the runner has not reached them, or they need another CLUSTER.", "", notRun, true)
	section("Not planned", "No unit in manifest.yaml yet; `go run ./release-test stale` lists them for the plan skill.", "", unplanned, true)
	section("Pass", "", "", pass, true)
	return b.String(), counts
}

// triageCost adds up what the triage calls reported in results/*/triage.json.
func triageCost(results string) float64 {
	files, _ := filepath.Glob(filepath.Join(results, "*", "triage.json"))
	total := 0.0
	for _, f := range files {
		var r struct {
			TotalCostUSD float64 `json:"total_cost_usd"`
		}
		if data, err := os.ReadFile(filepath.Clean(f)); err == nil && json.Unmarshal(data, &r) == nil {
			total += r.TotalCostUSD
		}
	}
	return total
}
