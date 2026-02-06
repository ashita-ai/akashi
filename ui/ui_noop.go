//go:build !ui

package ui

import "io/fs"

// DistFS returns nil when built without the ui tag.
// The server skips SPA mounting when the filesystem is nil.
func DistFS() (fs.FS, error) {
	return nil, nil
}
