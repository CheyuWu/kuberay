package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"sigs.k8s.io/yaml"
)

// Item is one entry of items.yaml: something a release is validated against.
type Item struct {
	ID        string   `json:"id"`
	Source    string   `json:"source,omitempty"`    // a docs.ray.io page or a path in this repository
	Procedure string   `json:"procedure,omitempty"` // what to check, for Items that are not a page or a sample
	Requires  []string `json:"requires,omitempty"`  // what a kind cluster cannot provide; a human runs the Item
}

// Unit is how manifest.yaml validates one Item.
type Unit struct {
	ID        string   `json:"id"`
	SourceSHA string   `json:"source_sha"`
	CoveredBy string   `json:"covered_by,omitempty"`
	Cluster   string   `json:"cluster,omitempty"`
	Notes     []string `json:"notes,omitempty"`
	Steps     []Step   `json:"steps,omitempty"`
	Cleanup   []string `json:"cleanup,omitempty"`
}

// Step passes when Run and then Check exit 0.
type Step struct {
	Name    string `json:"name,omitempty"`
	Run     string `json:"run"`
	Check   string `json:"check,omitempty"`
	Timeout int    `json:"timeout,omitempty"` // seconds, for Run and for Check each
}

const defaultTimeout = 300

// validID: ids name directories that are removed and recreated on every run.
var validID = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func loadItems(path string) ([]Item, error) {
	var f struct {
		Items []Item `json:"items"`
	}
	if err := readYAML(path, &f); err != nil {
		return nil, err
	}
	for _, it := range f.Items {
		if !validID.MatchString(it.ID) {
			return nil, fmt.Errorf("items.yaml: id %q is not a kebab-case slug", it.ID)
		}
	}
	return f.Items, nil
}

func loadManifest(path string) ([]Unit, error) {
	var f struct {
		Units []Unit `json:"units"`
	}
	err := readYAML(path, &f)
	return f.Units, err
}

// readYAML rejects unknown fields: a misspelled `chek:` must be an error, not a check that never runs.
func readYAML(path string, v any) error {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return err
	}
	if err := yaml.UnmarshalStrict(data, v); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return nil
}

func itemsByID(items []Item) map[string]Item {
	m := make(map[string]Item, len(items))
	for _, it := range items {
		m[it.ID] = it
	}
	return m
}

func unitsByID(units []Unit) map[string]Unit {
	m := make(map[string]Unit, len(units))
	for _, u := range units {
		m[u.ID] = u
	}
	return m
}
