package api

import (
	"embed"
	"io/fs"
)

// The frontend build (web/dist) is copied to internal/api/dist by the
// build script before `go build`. A placeholder index.html is committed
// so the daemon builds without a frontend toolchain present.
//
//go:embed all:dist
var webdist embed.FS

// UIFS returns the embedded frontend filesystem rooted at dist/.
func UIFS() fs.FS {
	sub, err := fs.Sub(webdist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
