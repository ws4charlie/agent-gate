// Package migrations embeds the numbered SQL files. Never edit a merged
// migration; add a new number instead.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
