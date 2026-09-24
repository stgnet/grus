// Package web holds the templates and static files, embedded in the binary
// so a deploy is copying one file.
package web

import "embed"

//go:embed templates static
var Files embed.FS
