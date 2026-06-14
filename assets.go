// Package websh embeds the web UI assets so the server ships as a single
// self-contained binary. The static/ directory is baked in at build time;
// `websh --static <dir>` overrides it with on-disk files for development.
package websh

import "embed"

//go:embed all:static
var staticFS embed.FS

// StaticFS is the embedded web UI (the contents of static/).
var StaticFS = staticFS
