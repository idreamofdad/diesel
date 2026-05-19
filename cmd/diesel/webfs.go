package main

import (
	"io/fs"

	dieselweb "diesel/web"
)

// embeddedWebFS returns the embedded SPA file tree. The embed lives in
// the diesel/web package so the frontend source/build can be shipped
// alongside the binary without `cmd/diesel` needing to know its
// directory structure. Returns nil if the embed is empty — the server
// then skips static serving, which is the right thing for "fresh clone
// with no `npm run build` yet" without making the binary fail.
func embeddedWebFS() fs.FS {
	return dieselweb.DistFS()
}
