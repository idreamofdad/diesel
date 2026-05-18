// Package web embeds the built Svelte frontend so the gin server can
// serve it without an external file dependency. The build output lives
// in `web/dist/` and is produced by `go generate ./...` (which runs
// `npm ci && npm run build` in this directory).
//
// Fresh checkouts only contain a stub index.html under web/dist (see
// the sentinel below) — that's enough for `go build` to succeed without
// npm being installed. Running `go generate ./...` replaces the stub
// with the real Vite output.
package web

import (
	"embed"
	"io/fs"
)

//go:generate sh -c "npm --prefix . ci && npm --prefix . run build"

//go:embed all:dist
var distFS embed.FS

// DistFS returns the embedded SPA tree rooted at `dist/` — what the
// gin server hands to http.FileServer. The sub-FS strips the leading
// "dist/" so URLs like /index.html and /assets/foo.js resolve cleanly.
func DistFS() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return distFS
	}
	return sub
}

// HasIndex reports whether the embedded dist actually contains a built
// frontend — true after `go generate ./...` has run, false on a fresh
// clone whose only dist content is the .gitkeep sentinel. The server
// uses this to decide between serving the real SPA and a stub page.
func HasIndex() bool {
	f, err := DistFS().Open("index.html")
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}
