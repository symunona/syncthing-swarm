//go:build !embedweb

// Package webui exposes the built frontend as an fs.FS. Default build has no
// embedded assets (FS is nil) — dev serves the UI from vite. See embed.go for
// the `-tags embedweb` production build.
package webui

import "io/fs"

// FS is the built web/dist tree, or nil when not embedded.
var FS fs.FS
