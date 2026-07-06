// Package migrations embeds the SQL migration files so the binary is
// self-contained: `pixledger` applies its own schema on boot.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
