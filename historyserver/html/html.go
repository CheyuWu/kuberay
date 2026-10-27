package html

import _ "embed"

// The cluster selection page and the files it loads.

//go:embed select_cluster.html
var ClusterSelectorHTML []byte

//go:embed select_cluster.css
var ClusterSelectorCSS []byte

//go:embed select_cluster.js
var ClusterSelectorJS []byte
