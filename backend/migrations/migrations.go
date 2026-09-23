// Package migrations embeds the numbered SQL migration files so the server
// binary can apply them at startup via golang-migrate. Files are applied in
// filename (lexicographic) order; never edit an already-applied file — add a
// new numbered one instead.
package migrations

import "embed"

// FS contains all *.sql migration files next to this file.
//
//go:embed *.sql
var FS embed.FS