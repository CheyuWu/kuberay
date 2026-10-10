package main

import (
	"context"
	"crypto/sha1" //nolint:gosec // git object ids are SHA-1; this is not a security boundary
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const docsMaster = "https://docs.ray.io/en/master/"

var (
	docsVersion = regexp.MustCompile(`^https://docs\.ray\.io/en/[^/]+/`)
	httpClient  = &http.Client{Timeout: time.Minute}
)

// source is what stale found for one Item.
type source struct {
	sha  string // git blob sha of the source
	file string // what the plan reads, relative to the repository
	dead string // set when the source is gone: the URL or path that no longer resolves
}

// cmdStale prints one line per Item whose unit is missing (new), outdated (changed) or whose source
// is gone (dead), and per unit without an Item (orphan).
func cmdStale(ctx context.Context, p paths, ids []string) error {
	items, err := loadItems(p.items)
	if err != nil {
		return err
	}
	units, err := loadManifest(p.manifest)
	if err != nil {
		return err
	}
	byID, planned := itemsByID(items), unitsByID(units)
	if len(ids) == 0 {
		for _, u := range units {
			if _, ok := byID[u.ID]; !ok {
				fmt.Println("orphan", u.ID)
			}
		}
		for _, it := range items {
			ids = append(ids, it.ID)
		}
	}
	for _, id := range ids {
		it, ok := byID[id]
		if !ok {
			return fmt.Errorf("no Item %s in items.yaml", id)
		}
		if len(it.Requires) > 0 {
			continue
		}
		src, err := p.fetchSource(ctx, it)
		if err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
		u, ok := planned[id]
		switch {
		case src.dead != "":
			fmt.Println("dead", id, src.dead)
		case !ok:
			fmt.Println("new", id, src.sha, src.file)
		case u.SourceSHA != src.sha:
			fmt.Println("changed", id, src.sha, src.file)
		}
	}
	return nil
}

// fetchSource hashes an Item's source: a docs page as its markdown on ray RAY_DOCS_REF (cached), a
// repository path at HEAD, a procedure as written.
func (p paths) fetchSource(ctx context.Context, it Item) (source, error) {
	switch {
	case it.Source == "":
		return source{sha: blobSHA([]byte(it.Procedure)), file: p.rel(p.items)}, nil
	case strings.HasPrefix(it.Source, "https://docs.ray.io/"):
		return p.fetchDocsPage(ctx, it)
	case strings.HasPrefix(it.Source, "https://"):
		body, _, found, err := fetch(ctx, it.Source)
		if err != nil {
			return source{}, err
		}
		if !found {
			return source{dead: it.Source}, nil
		}
		return p.cacheSource(it.ID+".src", body)
	default:
		out, err := exec.CommandContext(ctx, "git", "-C", p.root, "rev-parse", "-q", "--verify", "HEAD:"+it.Source).Output() //nolint:gosec // fixed tool, the argument is a path from items.yaml
		if err != nil {
			return source{dead: it.Source}, nil //nolint:nilerr // the path is not in HEAD, which is what dead means
		}
		return source{sha: strings.TrimSpace(string(out)), file: it.Source}, nil
	}
}

func (p paths) fetchDocsPage(ctx context.Context, it Item) (source, error) {
	// Pages move; follow docs.ray.io's redirects on master to find where the markdown is today.
	requested := docsVersion.ReplaceAllString(it.Source, docsMaster)
	requested, _, _ = strings.Cut(requested, "#")
	requested, _, _ = strings.Cut(requested, "?")
	_, final, found, err := fetch(ctx, requested)
	if err != nil {
		return source{}, err
	}
	page, ok := docsPagePath(requested, final)
	if !found || !ok {
		return source{dead: final}, nil
	}
	ref := os.Getenv("RAY_DOCS_REF")
	if ref == "" {
		ref = "master"
	}
	for _, ext := range []string{"md", "rst", "ipynb"} {
		body, _, found, err := fetch(ctx, fmt.Sprintf("https://raw.githubusercontent.com/ray-project/ray/%s/doc/source/%s.%s", ref, page, ext))
		if err != nil {
			return source{}, err
		}
		if found {
			return p.cacheSource(it.ID+"."+ext, body)
		}
	}
	return source{dead: final}, nil
}

// docsPagePath maps the URL docs.ray.io redirected to onto a path under doc/source/. ok is false
// when the page was removed: docs.ray.io then redirects to the section index with status 200.
func docsPagePath(requested, final string) (page string, ok bool) {
	page = strings.TrimSuffix(strings.TrimPrefix(final, docsMaster), ".html")
	removed := (page == "index" || strings.HasSuffix(page, "/index")) && !strings.HasSuffix(requested, "/index.html")
	return page, !removed
}

func (p paths) cacheSource(name string, body []byte) (source, error) {
	if err := os.MkdirAll(p.cache, 0o750); err != nil {
		return source{}, err
	}
	file := filepath.Join(p.cache, name)
	if err := os.WriteFile(file, body, 0o600); err != nil { //nolint:gosec // name is a validated Item id plus an extension
		return source{}, err
	}
	return source{sha: blobSHA(body), file: p.rel(file)}, nil
}

// fetch returns the body and the URL after redirects; found is false on 404. Any other non-200
// status is an error, so a rate limit or an outage is never taken for a removed page.
func fetch(ctx context.Context, url string) (body []byte, final string, found bool, err error) {
	status, body, final, err := get(ctx, url)
	switch {
	case err != nil:
		return nil, "", false, err
	case status == http.StatusNotFound:
		return nil, final, false, nil
	case status != http.StatusOK:
		return nil, "", false, fmt.Errorf("GET %s: HTTP %d", url, status)
	}
	return body, final, true, nil
}

func get(ctx context.Context, url string) (int, []byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil) //nolint:gosec // URLs come from items.yaml in this repository
	if err != nil {
		return 0, nil, "", err
	}
	resp, err := httpClient.Do(req) //nolint:gosec // see above
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, body, resp.Request.URL.String(), err
}

// blobSHA is git's blob object id, so `git hash-object` reproduces it.
func blobSHA(content []byte) string {
	h := sha1.New() //nolint:gosec // see the import
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}
