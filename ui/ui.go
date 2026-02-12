//go:build ui

package ui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// DistFS returns the embedded frontend filesystem rooted at the dist/ directory.
// Returns nil error on success.
func DistFS() (fs.FS, error) {
	return fs.Sub(distFS, "dist")
}
