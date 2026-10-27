package historyserver

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ray-project/kuberay/historyserver/html"
)

func TestSelectClusterFiles(t *testing.T) {
	mux := http.NewServeMux()
	for path, file := range selectClusterFiles {
		mux.Handle(path, file)
	}

	tests := map[string]struct {
		path        string
		contentType string
		body        []byte
	}{
		"page":       {path: "/select_cluster", contentType: "text/html; charset=utf-8", body: html.ClusterSelectorHTML},
		"stylesheet": {path: "/select_cluster/select_cluster.css", contentType: "text/css; charset=utf-8", body: html.ClusterSelectorCSS},
		"script":     {path: "/select_cluster/select_cluster.js", contentType: "text/javascript; charset=utf-8", body: html.ClusterSelectorJS},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, tc.path, nil)
			resp := httptest.NewRecorder()
			mux.ServeHTTP(resp, req)

			require.Equal(t, http.StatusOK, resp.Code)
			assert.Equal(t, tc.body, resp.Body.Bytes())
			assert.Equal(t, tc.contentType, resp.Header().Get("Content-Type"))
			assert.Equal(t, "nosniff", resp.Header().Get("X-Content-Type-Options"))
			assert.NotContains(t, resp.Header().Get("Content-Security-Policy"), "'unsafe-inline'")

			etag := resp.Header().Get("ETag")
			require.NotEmpty(t, etag)
			revalidate := httptest.NewRequestWithContext(t.Context(), http.MethodGet, tc.path, nil)
			revalidate.Header.Set("If-None-Match", etag)
			notModified := httptest.NewRecorder()
			mux.ServeHTTP(notModified, revalidate)

			assert.Equal(t, http.StatusNotModified, notModified.Code)
			assert.Empty(t, notModified.Body.Bytes())
		})
	}
}

// Every /select_cluster/ URL in the page must be one that routerSelectCluster serves.
func TestSelectClusterPageReferences(t *testing.T) {
	refs := regexp.MustCompile(`(?:href|src)="(/select_cluster/[^"]*)"`).FindAllSubmatch(html.ClusterSelectorHTML, -1)
	require.NotEmpty(t, refs)
	for _, ref := range refs {
		assert.Contains(t, selectClusterFiles, string(ref[1]))
	}
}
