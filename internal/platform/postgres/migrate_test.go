package postgres

import (
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
)

func mapFS(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

func TestLoadParsesAndOrdersMigrations(t *testing.T) {
	t.Parallel()

	m := NewMigrator(nil, mapFS(map[string]string{
		"0003_add_health_score.sql": "SELECT 3;",
		"0001_extensions.sql":       "SELECT 1;",
		"0002_service_catalog.sql":  "SELECT 2;",
		"README.md":                 "not a migration",
	}), nil)

	got, err := m.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("loaded %d migrations, want 3 (non-SQL files must be ignored)", len(got))
	}

	wantOrder := []int64{1, 2, 3}
	for i, mig := range got {
		if mig.Version != wantOrder[i] {
			t.Errorf("position %d has version %d, want %d", i, mig.Version, wantOrder[i])
		}
		if mig.Checksum == "" {
			t.Errorf("migration %s has no checksum", mig.Filename())
		}
	}
	if got[1].Name != "service_catalog" {
		t.Errorf("Name = %q, want service_catalog", got[1].Name)
	}
	if got[0].Filename() != "0001_extensions.sql" {
		t.Errorf("Filename() = %q", got[0].Filename())
	}
}

// Ordering is numeric, not lexicographic: version 10 must follow version 9.
func TestLoadOrdersNumericallyNotLexically(t *testing.T) {
	t.Parallel()

	m := NewMigrator(nil, mapFS(map[string]string{
		"0009_nine.sql":    "SELECT 9;",
		"0010_ten.sql":     "SELECT 10;",
		"0002_two.sql":     "SELECT 2;",
		"0100_hundred.sql": "SELECT 100;",
	}), nil)

	got, err := m.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := []int64{2, 9, 10, 100}
	for i, mig := range got {
		if mig.Version != want[i] {
			t.Fatalf("order = %v at position %d, want version %d", mig.Version, i, want[i])
		}
	}
}

// A migration skipped because of a typo is schema drift that surfaces later,
// somewhere worse. Loading must fail instead.
func TestLoadRejectsMalformedFilenames(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"missing version prefix": "add_users.sql",
		"too few digits":         "001_extensions.sql",
		"uppercase in name":      "0001_AddUsers.sql",
		"spaces in name":         "0001_add users.sql",
		"hyphenated name":        "0001_add-users.sql",
	}

	for name, filename := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := NewMigrator(nil, mapFS(map[string]string{filename: "SELECT 1;"}), nil)
			if _, err := m.Load(); err == nil {
				t.Errorf("Load() accepted malformed filename %q", filename)
			}
		})
	}
}

func TestLoadRejectsDuplicateVersions(t *testing.T) {
	t.Parallel()

	m := NewMigrator(nil, mapFS(map[string]string{
		"0001_extensions.sql": "SELECT 1;",
		"0001_catalog.sql":    "SELECT 2;",
	}), nil)

	_, err := m.Load()
	if err == nil {
		t.Fatal("Load() accepted two migrations with the same version")
	}
	if !strings.Contains(err.Error(), "version 1") {
		t.Errorf("error should name the duplicated version, got: %v", err)
	}
}

func TestLoadRejectsEmptyMigration(t *testing.T) {
	t.Parallel()

	m := NewMigrator(nil, mapFS(map[string]string{"0001_empty.sql": "  \n\t\n"}), nil)
	if _, err := m.Load(); err == nil {
		t.Error("Load() accepted an empty migration")
	}
}

func TestChecksumChangesWithContent(t *testing.T) {
	t.Parallel()

	first, err := NewMigrator(nil, mapFS(map[string]string{"0001_x.sql": "SELECT 1;"}), nil).Load()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewMigrator(nil, mapFS(map[string]string{"0001_x.sql": "SELECT 2;"}), nil).Load()
	if err != nil {
		t.Fatal(err)
	}

	if first[0].Checksum == second[0].Checksum {
		t.Error("different SQL produced the same checksum")
	}
}

// Editing an applied migration desynchronises every database except the
// author's. It must be a hard failure.
func TestVerifyChecksumsRejectsEditedMigration(t *testing.T) {
	t.Parallel()

	onDisk := []Migration{{Version: 1, Name: "extensions", Checksum: "new-checksum"}}
	applied := map[int64]AppliedMigration{
		1: {Version: 1, Name: "extensions", Checksum: "old-checksum", AppliedAt: time.Now()},
	}

	err := verifyChecksums(onDisk, applied)
	if err == nil {
		t.Fatal("verifyChecksums accepted an edited migration")
	}
	if got := errs.CodeOf(err); got != errs.CodeFailedPrecondition {
		t.Errorf("code = %q, want failed_precondition", got)
	}
	if !strings.Contains(errs.Message(err), "0001_extensions.sql") {
		t.Errorf("message should name the file, got: %q", errs.Message(err))
	}
}

func TestVerifyChecksumsAllowsUnchangedAndPending(t *testing.T) {
	t.Parallel()

	onDisk := []Migration{
		{Version: 1, Name: "extensions", Checksum: "abc"},
		{Version: 2, Name: "catalog", Checksum: "def"}, // pending, not yet applied
	}
	applied := map[int64]AppliedMigration{
		1: {Version: 1, Name: "extensions", Checksum: "abc"},
	}

	if err := verifyChecksums(onDisk, applied); err != nil {
		t.Errorf("verifyChecksums() error = %v, want nil", err)
	}
}

// The real migration set ships in the binary; it must always be loadable.
func TestEmbeddedMigrationsAreValid(t *testing.T) {
	t.Parallel()

	got, err := NewMigrator(nil, embeddedMigrations(t), nil).Load()
	if err != nil {
		t.Fatalf("the embedded migrations do not load: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no embedded migrations found")
	}
	if got[0].Version != 1 {
		t.Errorf("first migration is version %d, want 1", got[0].Version)
	}
}
