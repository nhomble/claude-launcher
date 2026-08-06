// Package web holds the UI: Go templates rendered server-side and the handful
// of static assets they need. Everything is embedded, so the launcher ships as
// a single self-contained binary with no runtime file dependencies.
package web

import "embed"

//go:embed templates/*.html
var Templates embed.FS

//go:embed static
var Static embed.FS
