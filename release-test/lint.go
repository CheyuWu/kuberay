package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// unpinnedVersion: KubeRay references that pin or float the version instead of using the env vars.
var unpinnedVersion = regexp.MustCompile(`ray-project/kuberay/(blob/|tree/)?(master|main|refs/(heads|tags)/|v[0-9])` +
	`|ray-project/kuberay[^ ]*(\?ref=|@)(master|main|latest|v[0-9])` +
	`|ray-project/kuberay/releases/download/v[0-9]` +
	`|quay\.io/kuberay/[a-z-]+:(latest|nightly|master|v[0-9])` +
	`|kuberay/(kuberay-operator|ray-cluster|kuberay-apiserver)( [^|;&]*)? --version[= ]+v?[0-9]`)

var errLint = errors.New("fix manifest.yaml first")

func cmdLint(p paths) error {
	problems := lintFiles(p)
	if len(problems) == 0 {
		fmt.Println("manifest.yaml: ok")
		return nil
	}
	printProblems(problems)
	return errLint
}

func printProblems(problems []string) {
	fmt.Fprintln(os.Stderr, "manifest.yaml:")
	for _, pr := range problems {
		fmt.Fprintln(os.Stderr, "  "+pr)
	}
}

func lintFiles(p paths) []string {
	items, err := loadItems(p.items)
	if err != nil {
		return []string{err.Error()}
	}
	units, err := loadManifest(p.manifest)
	if err != nil {
		return []string{err.Error()}
	}
	return lintManifest(items, units)
}

// lintManifest returns one line per problem, nil when the manifest is fine.
func lintManifest(items []Item, units []Unit) []string {
	var problems []string
	add := func(id, format string, args ...any) {
		problems = append(problems, id+": "+fmt.Sprintf(format, args...))
	}
	byID, byUnit, planned := itemsByID(items), unitsByID(units), map[string]bool{}
	for _, u := range units {
		if planned[u.ID] {
			add(u.ID, "duplicate unit")
		}
		planned[u.ID] = true
	}
	for _, u := range units {
		it, ok := byID[u.ID]
		switch {
		case !ok:
			add(u.ID, "not in items.yaml")
		case len(it.Requires) > 0:
			add(u.ID, "the Item has requires, so it must not have a unit")
		}
		if u.SourceSHA == "" {
			add(u.ID, "no source_sha")
		}
		target, hasTarget := byUnit[u.CoveredBy]
		switch {
		case u.CoveredBy == "" && len(u.Steps) == 0:
			add(u.ID, "no steps and no covered_by")
		case u.CoveredBy == "":
		case u.CoveredBy == u.ID:
			add(u.ID, "covered_by itself")
		case !hasTarget:
			add(u.ID, "covered_by %s, which has no unit", u.CoveredBy)
		case len(target.Steps) == 0:
			add(u.ID, "covered_by %s, which has no steps of its own; name the unit that runs them", u.CoveredBy)
		}
		commands := append([]string{}, u.Cleanup...)
		for i, s := range u.Steps {
			if strings.TrimSpace(s.Run) == "" {
				add(u.ID, "step %d has no run", i+1)
			}
			commands = append(commands, s.Run, s.Check)
		}
		for _, c := range commands {
			if c = strings.ReplaceAll(c, "\n", " "); unpinnedVersion.MatchString(c) {
				add(u.ID, "%s  <- pinned or unpinned KubeRay version; use $KUBERAY_VERSION or $CHART_VERSION", c)
			}
		}
	}
	return problems
}
