package postgres

import (
	"io/fs"
	"testing"

	"github.com/hexane/atlas/migrations"
)

// embeddedMigrations returns the real migration set shipped in the binary,
// rooted so filenames appear at the top level exactly as [Migrator] expects.
func embeddedMigrations(t *testing.T) fs.FS {
	t.Helper()
	return migrations.FS
}
