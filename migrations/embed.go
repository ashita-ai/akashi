// Package migrations embeds SQL migration files for use at runtime.
// Migrations are embedded so they work regardless of working directory.
package migrations

import "embed"

// FS is the embedded migrations filesystem.
// Contains all .sql files in this directory (e.g. 001_initial.sql).
//
//go:embed *.sql
var FS embed.FS
