// Package web holds the gateway's web UI and the built bundle the gateway
// serves. The bundle is built, not committed; web/dist/.gitkeep keeps the
// embed glob satisfied on a fresh checkout, so `go build` works before Bun
// has ever run.
package web

import "embed"

//go:embed all:dist
var Assets embed.FS
