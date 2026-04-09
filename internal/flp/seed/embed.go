package seed

import "embed"

// SeedFS embeds all JSON seed data files for use at runtime.
// These files are compiled into the binary and available without filesystem access.
//
//go:embed *.json
var SeedFS embed.FS
