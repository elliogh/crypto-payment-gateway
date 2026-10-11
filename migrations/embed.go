// Package migrations provides SQL migrations embedded in the application binary.
package migrations

import "embed"

// FS contains the same SQL files used by the Goose CLI from the repository root.
//
//go:embed *.sql
var FS embed.FS
