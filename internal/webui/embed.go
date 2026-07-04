//go:build embedweb

// Production frontend embed. Run `npm --prefix web run build` first (vite
// writes to internal/webui/dist), then `go build -tags embedweb ./cmd/swarmd`.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var embedded embed.FS

// FS is the embedded web/dist tree.
var FS fs.FS

func init() {
	sub, err := fs.Sub(embedded, "dist")
	if err != nil {
		panic(err)
	}
	FS = sub
}
