// Package migrations embeds Atlas's SQL schema migrations into the binary.
//
// Embedding rather than shipping a directory means a deployed atlas-server can
// never be paired with the wrong migration set: the schema it expects and the
// migrations that produce it are the same artifact. It also removes any
// filesystem path from the deployment contract, which matters for the
// scratch-based container image.
//
// Files are named NNNN_lower_snake_case.sql and are forward-only. See
// docs/adr/0007-forward-only-migrations.md for why there are no down files,
// and docs/database/schema.md for the schema itself.
package migrations

import "embed"

// FS holds every migration, applied in filename order.
//
//go:embed *.sql
var FS embed.FS
