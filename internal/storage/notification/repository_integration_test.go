//go:build integration

package notification_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	corenotification "github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/log"
	"github.com/hexane/atlas/internal/platform/postgres"
	"github.com/hexane/atlas/internal/platform/secretbox"
	"github.com/hexane/atlas/internal/storage/notification"
	"github.com/hexane/atlas/migrations"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testDatabaseURLEnv = "ATLAS_TEST_DATABASE_URL"

// newDatabase migrates a fresh database and returns its pool.
func newDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := freshUnmigrated(t)
	if _, err := postgres.NewMigrator(pool, migrations.FS, log.Discard()).Apply(context.Background()); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return pool
}

// freshUnmigrated creates an empty database and returns its pool.
func freshUnmigrated(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}
	parsed, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse %s: %v", testDatabaseURLEnv, err)
	}
	admin, err := pgxpool.NewWithConfig(context.Background(), parsed)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close()

	name := fmt.Sprintf("atlas_notification_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(context.Background(), "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		cleanup, err := pgxpool.NewWithConfig(context.Background(), parsed)
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})

	cfg := config.Default()
	cfg.Database.Host = parsed.ConnConfig.Host
	cfg.Database.Port = int(parsed.ConnConfig.Port)
	cfg.Database.Name = name
	cfg.Database.User = parsed.ConnConfig.User
	cfg.Database.Password = parsed.ConnConfig.Password
	cfg.Database.SSLMode = "disable"

	pool := postgres.NewPool(cfg.Database, log.Discard())
	ctx := context.Background()
	if err := pool.Start(ctx); err != nil {
		t.Fatalf("pool start: %v", err)
	}
	t.Cleanup(func() { _ = pool.Stop(context.Background()) })
	return pool.DB()
}

func newKey(t *testing.T) *secretbox.Key {
	t.Helper()
	raw := make([]byte, secretbox.KeySize)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	k, err := secretbox.NewKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func newRepository(t *testing.T) (*notification.Repository, *pgxpool.Pool) {
	t.Helper()
	pool := newDatabase(t)
	return notification.NewRepository(pool, newKey(t)), pool
}

func ptr[T any](v T) *T { return &v }

func channelInput(name string, secret *string) corenotification.ChannelInput {
	return corenotification.ChannelInput{
		Name: name, Type: corenotification.ChannelEmail, Enabled: true, MinSeverity: corenotification.SeverityWarning,
		Config: map[string]any{"host": "smtp.example.invalid", "port": float64(587), "to": []any{"ops@example.invalid"}},
		Secret: secret,
	}
}

func seedNode(t *testing.T, pool *pgxpool.Pool, nodeID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO nodes (node_id, hostname, last_seen_at) VALUES ($1, $1, now())`, nodeID); err != nil {
		t.Fatalf("seed node: %v", err)
	}
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// --- channels and secrets ---------------------------------------------------

func TestChannelSecretRoundTrips(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()
	const secret = "gmail-app-password-abcd"

	c, err := repo.CreateChannel(ctx, channelInput("ops", ptr(secret)))
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if !c.SecretConfigured {
		t.Error("SecretConfigured = false after creating with a secret")
	}
	got, err := repo.ChannelSecret(ctx, c.ID)
	if err != nil {
		t.Fatalf("ChannelSecret: %v", err)
	}
	if got != secret {
		t.Errorf("ChannelSecret = %q, want the stored secret", got)
	}

	var keyID string
	if err := pool.QueryRow(ctx, `SELECT secret_key_id FROM notification_channels WHERE id = $1`, c.ID).Scan(&keyID); err != nil {
		t.Fatal(err)
	}
	other := notification.NewRepository(pool, nil)
	if _, err := other.ChannelSecret(ctx, c.ID); errs.CodeOf(err) != errs.CodeUnavailable {
		t.Errorf("ChannelSecret with no key: %v, want unavailable", err)
	}
	if keyID == "" || len(keyID) != 16 {
		t.Errorf("secret_key_id = %q, want a 16-hex-char key id", keyID)
	}

	got2, err := repo.GetChannel(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got2.Config["host"] != "smtp.example.invalid" || got2.MinSeverity != corenotification.SeverityWarning {
		t.Errorf("GetChannel = %+v, config or min_severity lost", got2)
	}
}

// The plaintext appears nowhere in the stored row, in any encoding Postgres
// would render it in.
func TestStoredRowNeverContainsThePlaintext(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()
	const secret = "https://prod.example.invalid/workflows/abc?sig=PLAINTEXT-SIG-VALUE"

	c, err := repo.CreateChannel(ctx, channelInput("teams-ops", ptr(secret)))
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	var row string
	if err := pool.QueryRow(ctx, `SELECT to_jsonb(c)::text FROM notification_channels c WHERE id = $1`, c.ID).Scan(&row); err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{secret, "PLAINTEXT-SIG-VALUE", hex.EncodeToString([]byte("PLAINTEXT-SIG-VALUE"))} {
		if strings.Contains(strings.ToLower(row), strings.ToLower(needle)) {
			t.Fatalf("the stored row contains %q: %s", needle, row)
		}
	}
	if !strings.Contains(row, `"secret_ciphertext": "\\x`) {
		t.Errorf("no ciphertext stored: %s", row)
	}
}

// The channel id is the associated data: ciphertext moved to another
// channel's row does not decrypt there.
func TestCiphertextCopiedToAnotherChannelDoesNotDecrypt(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()
	const secret = "s3cret-for-a"

	a, err := repo.CreateChannel(ctx, channelInput("a", ptr(secret)))
	if err != nil {
		t.Fatal(err)
	}
	b, err := repo.CreateChannel(ctx, channelInput("b", nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE notification_channels b SET secret_ciphertext = a.secret_ciphertext,
		secret_key_id = a.secret_key_id FROM notification_channels a WHERE a.id = $1 AND b.id = $2`, a.ID, b.ID); err != nil {
		t.Fatal(err)
	}

	_, err = repo.ChannelSecret(ctx, b.ID)
	if !errors.Is(err, corenotification.ErrSecretUnreadable) || !errors.Is(err, secretbox.ErrDecrypt) {
		t.Fatalf("ChannelSecret(b) = %v, want ErrSecretUnreadable wrapping ErrDecrypt", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the error carries the secret: %v", err)
	}
}

func TestWrongKeyIsAKeyMismatchWithoutSecretMaterial(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()
	const secret = "s3cret-under-key-1"

	c, err := repo.CreateChannel(ctx, channelInput("ops", ptr(secret)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = notification.NewRepository(pool, newKey(t)).ChannelSecret(ctx, c.ID)
	if !errors.Is(err, secretbox.ErrKeyMismatch) || !errors.Is(err, corenotification.ErrSecretUnreadable) {
		t.Fatalf("err = %v, want ErrSecretUnreadable wrapping ErrKeyMismatch", err)
	}
	if errs.CodeOf(err) != errs.CodeFailedPrecondition {
		t.Errorf("code = %s, want failed_precondition", errs.CodeOf(err))
	}
	if msg := err.Error(); strings.Contains(msg, secret) || !strings.Contains(msg, "encryption key does not match") {
		t.Errorf("error = %q: must say the key does not match and carry no secret", msg)
	}
}

// No reader path returns the secret: list, get, and every error a channel
// operation can return.
func TestSecretNeverAppearsInReadsOrErrors(t *testing.T) {
	repo, _ := newRepository(t)
	ctx := context.Background()
	const secret = "never-show-me-0123"

	c, err := repo.CreateChannel(ctx, channelInput("ops", ptr(secret)))
	if err != nil {
		t.Fatal(err)
	}
	list, err := repo.ListChannels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetChannel(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, dupErr := repo.CreateChannel(ctx, channelInput("ops", ptr(secret)))
	if errs.CodeOf(dupErr) != errs.CodeAlreadyExists {
		t.Errorf("duplicate name: %v, want already_exists", dupErr)
	}
	_, missingErr := repo.UpdateChannel(ctx, "missing", channelInput("x", ptr(secret)))
	_, invalidErr := repo.CreateChannel(ctx, corenotification.ChannelInput{Name: "bad", Type: "webhook", Secret: ptr(secret)})
	_, nokeyErr := notification.NewRepository(nil, nil).CreateChannel(ctx, channelInput("nokey", ptr(secret)))
	if errs.CodeOf(nokeyErr) != errs.CodeUnavailable {
		t.Errorf("create with a secret and no key: %v, want unavailable", nokeyErr)
	}

	for name, out := range map[string]string{
		"list":            fmt.Sprintf("%+v", list),
		"get":             fmt.Sprintf("%+v", got),
		"create response": fmt.Sprintf("%+v", c),
		"duplicate error": fmt.Sprint(dupErr),
		"missing error":   fmt.Sprint(missingErr),
		"invalid error":   fmt.Sprint(invalidErr),
		"no-key error":    fmt.Sprint(nokeyErr),
	} {
		if strings.Contains(out, secret) {
			t.Errorf("%s contains the secret: %s", name, out)
		}
	}
}

func TestUpdateWithoutASecretKeepsTheStoredOne(t *testing.T) {
	repo, _ := newRepository(t)
	ctx := context.Background()

	c, err := repo.CreateChannel(ctx, channelInput("ops", ptr("first")))
	if err != nil {
		t.Fatal(err)
	}
	in := channelInput("ops-renamed", nil)
	in.MinSeverity = corenotification.SeverityCritical
	updated, err := repo.UpdateChannel(ctx, c.ID, in)
	if err != nil {
		t.Fatalf("UpdateChannel: %v", err)
	}
	if !updated.SecretConfigured || updated.Name != "ops-renamed" || updated.MinSeverity != corenotification.SeverityCritical {
		t.Errorf("updated = %+v", updated)
	}
	if got, err := repo.ChannelSecret(ctx, c.ID); err != nil || got != "first" {
		t.Fatalf("after an update with no secret: %q, %v; want the original", got, err)
	}

	if _, err := repo.UpdateChannel(ctx, c.ID, channelInput("ops-renamed", ptr("second"))); err != nil {
		t.Fatal(err)
	}
	if got, err := repo.ChannelSecret(ctx, c.ID); err != nil || got != "second" {
		t.Fatalf("after replacing the secret: %q, %v; want the new one", got, err)
	}
}

func TestChannelChecksRejectBadRows(t *testing.T) {
	_, pool := newRepository(t)
	ctx := context.Background()
	for name, q := range map[string]string{
		"unknown min_severity": `INSERT INTO notification_channels (id, name, type, min_severity) VALUES ('x1', 'x1', 'email', 'info')`,
		"ciphertext without key id": `INSERT INTO notification_channels (id, name, type, secret_ciphertext)
			VALUES ('x2', 'x2', 'email', '\x00')`,
		"config not an object": `INSERT INTO notification_channels (id, name, type, config) VALUES ('x3', 'x3', 'email', '[]')`,
	} {
		if _, err := pool.Exec(ctx, q); sqlState(err) != "23514" {
			t.Errorf("%s: err = %v, want a check violation", name, err)
		}
	}
}

// --- rules --------------------------------------------------------------------

func TestSeededGlobalRules(t *testing.T) {
	repo, _ := newRepository(t)
	ctx := context.Background()

	for _, tc := range []struct {
		trigger    corenotification.Trigger
		warn, crit *float64
		forSeconds int
		hysteresis float64
	}{
		{corenotification.TriggerDiskPct, ptr(80.0), ptr(90.0), 300, 5},
		{corenotification.TriggerRAMPct, ptr(85.0), ptr(95.0), 300, 5},
		{corenotification.TriggerNodeDown, nil, nil, 480, 0},
		{corenotification.TriggerServiceFailed, nil, nil, 300, 0},
		{corenotification.TriggerContainerExit, nil, nil, 0, 0},
		{corenotification.TriggerContainerRestart, nil, nil, 0, 0},
	} {
		r, err := repo.EffectiveRule(ctx, tc.trigger, "")
		if err != nil {
			t.Fatalf("EffectiveRule(%s): %v", tc.trigger, err)
		}
		eq := func(a, b *float64) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }
		if !r.Enabled || !eq(r.WarnThreshold, tc.warn) || !eq(r.CritThreshold, tc.crit) || r.ForSeconds != tc.forSeconds ||
			r.Hysteresis != tc.hysteresis || r.RecoverySeconds != 120 || r.CooldownSeconds != 1800 || !r.SendRecovery {
			t.Errorf("%s = %+v", tc.trigger, r)
		}
	}
	for _, trigger := range []corenotification.Trigger{corenotification.TriggerWebsiteDown, corenotification.TriggerWebsiteCertExpiry} {
		r, err := repo.EffectiveRule(ctx, trigger, "")
		if err != nil || !r.Enabled || r.ForSeconds != 0 || r.RecoverySeconds != 0 || r.CooldownSeconds != 1800 || !r.SendRecovery {
			t.Errorf("%s = %+v, %v; want enabled with zero holds", trigger, r, err)
		}
	}
	rules, err := repo.ListRules(ctx)
	if err != nil || len(rules) != 8 {
		t.Fatalf("ListRules = %d rules, %v; want the 8 seeded globals", len(rules), err)
	}
}

// Each override field replaces only itself; everything else is inherited.
func TestEffectiveRuleMergesTheOverridePerField(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()
	seedNode(t, pool, "node-1")
	seedNode(t, pool, "node-2")

	for _, tc := range []struct {
		name     string
		override corenotification.RuleSpec
		check    func(r corenotification.Rule) bool
	}{
		{"warn only", corenotification.RuleSpec{WarnThreshold: ptr(70.0)}, func(r corenotification.Rule) bool {
			return *r.WarnThreshold == 70 && *r.CritThreshold == 90 && r.ForSeconds == 300 && r.Enabled
		}},
		{"crit and for", corenotification.RuleSpec{CritThreshold: ptr(97.0), ForSeconds: ptr(60)}, func(r corenotification.Rule) bool {
			return *r.WarnThreshold == 80 && *r.CritThreshold == 97 && r.ForSeconds == 60 && r.CooldownSeconds == 1800
		}},
		{"disabled", corenotification.RuleSpec{Enabled: ptr(false)}, func(r corenotification.Rule) bool {
			return !r.Enabled && *r.WarnThreshold == 80 && r.SendRecovery
		}},
		{"zero hysteresis, no recovery message", corenotification.RuleSpec{Hysteresis: ptr(0.0), SendRecovery: ptr(false)},
			func(r corenotification.Rule) bool {
				return r.Hysteresis == 0 && !r.SendRecovery && r.RecoverySeconds == 120
			}},
		{"excludes", corenotification.RuleSpec{SubjectExcludes: []string{"/boot"}}, func(r corenotification.Rule) bool {
			return len(r.SubjectExcludes) == 1 && r.SubjectExcludes[0] == "/boot" && *r.WarnThreshold == 80
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := tc.override
			spec.Trigger, spec.NodeID = corenotification.TriggerDiskPct, "node-1"
			if err := repo.SetRule(ctx, spec, "test"); err != nil {
				t.Fatalf("SetRule: %v", err)
			}
			r, err := repo.EffectiveRule(ctx, corenotification.TriggerDiskPct, "node-1")
			if err != nil {
				t.Fatalf("EffectiveRule: %v", err)
			}
			if !tc.check(r) {
				t.Errorf("effective = %+v", r)
			}
			// Another node is unaffected.
			other, err := repo.EffectiveRule(ctx, corenotification.TriggerDiskPct, "node-2")
			if err != nil || len(other.Overridden) != 0 || *other.WarnThreshold != 80 {
				t.Errorf("node-2 = %+v, %v; want the plain global", other, err)
			}
		})
	}

	// The override row stores NULL for every field it did not set.
	var nulls int
	if err := pool.QueryRow(ctx, `SELECT num_nulls(enabled, warn_threshold, crit_threshold, for_seconds, recovery_seconds,
		hysteresis, cooldown_seconds, send_recovery) FROM notification_rules WHERE node_id = 'node-1'`).Scan(&nulls); err != nil {
		t.Fatal(err)
	}
	if nulls != 8 {
		t.Errorf("override row has %d NULL fields, want 8 (only subject_excludes set by the last case)", nulls)
	}

	if err := repo.DeleteRule(ctx, corenotification.TriggerDiskPct, "node-1"); err != nil {
		t.Fatalf("DeleteRule(override): %v", err)
	}
	if r, err := repo.EffectiveRule(ctx, corenotification.TriggerDiskPct, "node-1"); err != nil || len(r.Overridden) != 0 {
		t.Errorf("after deleting the override: %+v, %v", r, err)
	}
}

func TestGlobalRuleCheckRejectsNulls(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()
	seedNode(t, pool, "node-1")

	for _, col := range []string{"enabled", "for_seconds", "recovery_seconds", "hysteresis", "cooldown_seconds",
		"send_recovery", "subject_excludes", "warn_threshold", "crit_threshold"} {
		_, err := pool.Exec(ctx, `UPDATE notification_rules SET `+col+` = NULL WHERE trigger = 'disk_pct' AND node_id IS NULL`)
		if sqlState(err) != "23514" {
			t.Errorf("NULL %s on the global disk rule: err = %v, want a check violation", col, err)
		}
	}
	// node_down's global carries no thresholds, and that is valid.
	var warn *float64
	if err := pool.QueryRow(ctx, `SELECT warn_threshold FROM notification_rules WHERE trigger = 'node_down' AND node_id IS NULL`).Scan(&warn); err != nil || warn != nil {
		t.Errorf("node_down warn_threshold = %v, %v; want NULL", warn, err)
	}
	// An override row may be all NULL.
	if _, err := pool.Exec(ctx, `INSERT INTO notification_rules (id, trigger, node_id) VALUES ('o1', 'disk_pct', 'node-1')`); err != nil {
		t.Errorf("an all-NULL override was rejected: %v", err)
	}
	// One rule per (trigger, node), and one global per trigger.
	if _, err := pool.Exec(ctx, `INSERT INTO notification_rules (id, trigger, node_id) VALUES ('o2', 'disk_pct', 'node-1')`); sqlState(err) != "23505" {
		t.Errorf("a second override for the same node: err = %v, want unique violation", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO notification_rules (id, trigger, enabled, warn_threshold, crit_threshold, for_seconds,
		recovery_seconds, hysteresis, cooldown_seconds, send_recovery, subject_excludes)
		VALUES ('g2', 'disk_pct', true, 1, 2, 0, 0, 0, 0, true, '{}')`); sqlState(err) != "23505" {
		t.Errorf("a second global: err = %v, want unique violation", err)
	}
	// The repository refuses an incomplete global before it reaches the database.
	if err := repo.SetRule(ctx, corenotification.RuleSpec{Trigger: corenotification.TriggerDiskPct, WarnThreshold: ptr(1.0)}, "t"); errs.CodeOf(err) != errs.CodeInvalidArgument {
		t.Errorf("SetRule(incomplete global) = %v, want invalid_argument", err)
	}
}

func TestSetRuleGuardsTheEffectiveRule(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()
	seedNode(t, pool, "node-1")

	// warn 95 over an inherited crit 90 is refused.
	if err := repo.SetRule(ctx, corenotification.RuleSpec{Trigger: corenotification.TriggerDiskPct, NodeID: "node-1",
		WarnThreshold: ptr(95.0)}, "t"); errs.CodeOf(err) != errs.CodeInvalidArgument {
		t.Errorf("override warn above inherited crit: %v, want invalid_argument", err)
	}
	if err := repo.SetRule(ctx, corenotification.RuleSpec{Trigger: corenotification.TriggerDiskPct, NodeID: "unknown",
		ForSeconds: ptr(1)}, "t"); errs.CodeOf(err) != errs.CodeNotFound {
		t.Errorf("override for an unknown node: %v, want not_found", err)
	}

	if err := repo.SetRule(ctx, corenotification.RuleSpec{Trigger: corenotification.TriggerDiskPct, NodeID: "node-1",
		CritThreshold: ptr(99.0), WarnThreshold: ptr(92.0)}, "t"); err != nil {
		t.Fatal(err)
	}
	// A new global whose crit would fall below the override's warn is refused.
	global := corenotification.RuleSpec{
		Trigger: corenotification.TriggerDiskPct, Enabled: ptr(true), WarnThreshold: ptr(50.0), CritThreshold: ptr(60.0),
		ForSeconds: ptr(300), RecoverySeconds: ptr(120), Hysteresis: ptr(5.0), CooldownSeconds: ptr(1800),
		SendRecovery: ptr(true), SubjectExcludes: []string{},
	}
	global.CritThreshold = ptr(91.0)
	global.WarnThreshold = ptr(93.0)
	if err := repo.SetRule(ctx, global, "t"); err == nil {
		t.Error("an invalid global was accepted")
	}
	global.WarnThreshold, global.CritThreshold = ptr(70.0), ptr(80.0)
	if err := repo.SetRule(ctx, global, "t"); err != nil {
		t.Fatalf("SetRule(global): %v", err)
	}
	r, _ := repo.EffectiveRule(ctx, corenotification.TriggerDiskPct, "")
	if *r.WarnThreshold != 70 || *r.CritThreshold != 80 {
		t.Errorf("global = %+v", r)
	}
	if err := repo.DeleteRule(ctx, corenotification.TriggerDiskPct, ""); errs.CodeOf(err) != errs.CodeFailedPrecondition {
		t.Errorf("deleting a global that has overrides: %v, want failed_precondition", err)
	}
	if err := repo.DeleteRule(ctx, corenotification.TriggerRAMPct, ""); err != nil {
		t.Errorf("deleting a global with no overrides: %v", err)
	}
	if _, err := repo.EffectiveRule(ctx, corenotification.TriggerRAMPct, "node-1"); errs.CodeOf(err) != errs.CodeNotFound {
		t.Errorf("EffectiveRule with no global: %v, want not_found", err)
	}
}

// --- state --------------------------------------------------------------------

func TestStateUpsertGetAndListActive(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()
	seedNode(t, pool, "node-1")
	now := time.Now().UTC().Truncate(time.Microsecond)

	if _, found, err := repo.GetState(ctx, corenotification.TriggerDiskPct, "node-1", "/"); err != nil || found {
		t.Fatalf("GetState before any write = %v, %v", found, err)
	}
	s := corenotification.State{
		Trigger: corenotification.TriggerDiskPct, NodeID: "node-1", Subject: "/", State: corenotification.StateFiring,
		Severity: corenotification.SeverityCritical, Value: ptr(93.5), ConditionSince: &now, StateSince: now,
		LastNotifiedAt: &now, LastNotifiedKind: corenotification.KindFiring, FlapCount: 1,
		Baseline: map[string]any{"restart_count": float64(3)},
	}
	if err := repo.UpsertState(ctx, s); err != nil {
		t.Fatalf("UpsertState: %v", err)
	}
	if err := repo.UpsertState(ctx, corenotification.State{Trigger: corenotification.TriggerDiskPct, NodeID: "node-1",
		Subject: "/data", State: corenotification.StateOK, StateSince: now}); err != nil {
		t.Fatal(err)
	}

	got, found, err := repo.GetState(ctx, corenotification.TriggerDiskPct, "node-1", "/")
	if err != nil || !found {
		t.Fatalf("GetState = %v, %v", found, err)
	}
	if got.State != corenotification.StateFiring || *got.Value != 93.5 || !got.ConditionSince.Equal(now) ||
		got.LastNotifiedKind != corenotification.KindFiring || got.Baseline["restart_count"] != float64(3) {
		t.Errorf("GetState = %+v", got)
	}

	active, err := repo.ListActiveStates(ctx)
	if err != nil || len(active) != 1 || active[0].Subject != "/" {
		t.Fatalf("ListActiveStates = %+v, %v; want only the firing /", active, err)
	}

	s.State, s.Severity, s.Value = corenotification.StateOK, "", nil
	if err := repo.UpsertState(ctx, s); err != nil {
		t.Fatal(err)
	}
	if active, _ := repo.ListActiveStates(ctx); len(active) != 0 {
		t.Errorf("after recovery ListActiveStates = %+v", active)
	}
}

// TestResetNodeStatesClearsEveryNonOkRowForNode covers the fix round's
// item 3: ResetNodeStates must reset every trigger and subject for its
// node in one statement — not just subject "" as an earlier version of
// the evaluator's own mute-ended reset mistakenly did — while leaving an
// already-ok row and any other node's rows completely untouched.
func TestResetNodeStatesClearsEveryNonOkRowForNode(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()
	seedNode(t, pool, "node-1")
	seedNode(t, pool, "node-2")
	frozenAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)

	firing := func(nodeID, subject string) corenotification.State {
		return corenotification.State{
			Trigger: corenotification.TriggerDiskPct, NodeID: nodeID, Subject: subject,
			State: corenotification.StateFiring, Severity: corenotification.SeverityCritical,
			Value: ptr(93.5), ConditionSince: &frozenAt, StateSince: frozenAt, FlapCount: 2,
			FlapWindowStart: &frozenAt,
		}
	}
	for _, s := range []corenotification.State{
		firing("node-1", "/"),
		firing("node-1", "/data"),
		{Trigger: corenotification.TriggerNodeDown, NodeID: "node-1", State: corenotification.StateOK, StateSince: frozenAt},
		firing("node-2", "/"),
	} {
		if err := repo.UpsertState(ctx, s); err != nil {
			t.Fatalf("seed UpsertState(%s/%s): %v", s.NodeID, s.Subject, err)
		}
	}

	if err := repo.ResetNodeStates(ctx, "node-1"); err != nil {
		t.Fatalf("ResetNodeStates: %v", err)
	}

	for _, subject := range []string{"/", "/data"} {
		got, found, err := repo.GetState(ctx, corenotification.TriggerDiskPct, "node-1", subject)
		if err != nil || !found {
			t.Fatalf("GetState(node-1, %q): found=%v err=%v", subject, found, err)
		}
		if got.State != corenotification.StateOK || got.Severity != "" || got.Value != nil ||
			got.ConditionSince != nil || got.FlapCount != 0 || got.FlapWindowStart != nil {
			t.Errorf("node-1/%q after reset = %+v, want ok with severity/value/condition_since/flap counters cleared", subject, got)
		}
		if !got.StateSince.After(frozenAt) {
			t.Errorf("node-1/%q state_since = %v, want refreshed to the reset time, not left at %v", subject, got.StateSince, frozenAt)
		}
	}

	// Already ok before the reset: untouched (state_since does not move).
	okBefore, _, _ := repo.GetState(ctx, corenotification.TriggerNodeDown, "node-1", "")
	if !okBefore.StateSince.Equal(frozenAt) {
		t.Errorf("node-1's already-ok node_down state_since = %v, want untouched at %v", okBefore.StateSince, frozenAt)
	}

	// A different node's firing row must survive node-1's reset entirely.
	other, _, _ := repo.GetState(ctx, corenotification.TriggerDiskPct, "node-2", "/")
	if other.State != corenotification.StateFiring || other.Severity != corenotification.SeverityCritical {
		t.Errorf("node-2 state = %+v, want untouched firing/critical — ResetNodeStates must be scoped to its own node", other)
	}
}

// --- deliveries ---------------------------------------------------------------

func notificationFor(eventID string) corenotification.Notification {
	return corenotification.Notification{
		EventID: eventID, Kind: corenotification.KindFiring, Trigger: corenotification.TriggerDiskPct,
		NodeID: "node-1", Subject: "/", Severity: corenotification.SeverityCritical,
		Title: "Disk / at 93%", Message: "node-1: / is 93% full",
	}
}

func TestEnqueueDedupesOnEventAndChannel(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()
	a, _ := repo.CreateChannel(ctx, channelInput("a", nil))
	b, _ := repo.CreateChannel(ctx, channelInput("b", nil))

	n, err := repo.Enqueue(ctx, notificationFor("evt-1"), []corenotification.Channel{a, b})
	if err != nil || n != 2 {
		t.Fatalf("first Enqueue = %d, %v; want 2", n, err)
	}
	n, err = repo.Enqueue(ctx, notificationFor("evt-1"), []corenotification.Channel{a, b})
	if err != nil || n != 0 {
		t.Fatalf("repeat Enqueue = %d, %v; want 0", n, err)
	}
	n, err = repo.Enqueue(ctx, notificationFor("evt-2"), []corenotification.Channel{a})
	if err != nil || n != 1 {
		t.Fatalf("new event = %d, %v; want 1", n, err)
	}
	var rows int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM notification_deliveries`).Scan(&rows)
	if rows != 3 {
		t.Errorf("rows = %d, want 3", rows)
	}
	page, err := repo.ListDeliveries(ctx, corenotification.DeliveryFilter{ChannelID: a.ID})
	if err != nil || len(page.Deliveries) != 2 || page.Deliveries[0].ChannelType != corenotification.ChannelEmail {
		t.Errorf("deliveries for a = %+v, %v", page.Deliveries, err)
	}
}

func TestListDeliveriesKeysetPagination(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()
	c, _ := repo.CreateChannel(ctx, channelInput("a", nil))
	for i := range 5 {
		if _, err := repo.Enqueue(ctx, notificationFor(fmt.Sprintf("evt-%d", i)), []corenotification.Channel{c}); err != nil {
			t.Fatal(err)
		}
	}
	// Two rows share a created_at, so the id tie-break is exercised.
	if _, err := pool.Exec(ctx, `UPDATE notification_deliveries SET created_at = '2026-01-01T00:00:00Z' WHERE event_id IN ('evt-1', 'evt-2')`); err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	var filter corenotification.DeliveryFilter
	filter.Limit = 2
	var sizes []int
	for range 5 {
		page, err := repo.ListDeliveries(ctx, filter)
		if err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, len(page.Deliveries))
		for _, d := range page.Deliveries {
			if seen[d.ID] {
				t.Fatalf("delivery %s returned twice", d.ID)
			}
			seen[d.ID] = true
		}
		if page.Next == nil {
			break
		}
		filter.Before = page.Next
	}
	if len(seen) != 5 || fmt.Sprint(sizes) != "[2 2 1]" {
		t.Errorf("pages %v covering %d deliveries, want [2 2 1] covering 5", sizes, len(seen))
	}

	all, _ := repo.ListDeliveries(ctx, corenotification.DeliveryFilter{})
	if err := repo.MarkDelivered(ctx, all.Deliveries[0].ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	delivered, _ := repo.ListDeliveries(ctx, corenotification.DeliveryFilter{Status: corenotification.StatusDelivered})
	if len(delivered.Deliveries) != 1 || delivered.Deliveries[0].DeliveredAt == nil {
		t.Errorf("delivered filter = %+v", delivered.Deliveries)
	}
	if err := repo.MarkDelivered(ctx, all.Deliveries[0].ID, time.Now()); errs.CodeOf(err) != errs.CodeNotFound {
		t.Errorf("marking a finished delivery again: %v, want not_found", err)
	}
}

// A send error is stored redacted (URL query secrets included) and cut to
// the column's CHECK length.
func TestMarkRetryAndExhaustedStoreARedactedTruncatedError(t *testing.T) {
	repo, _ := newRepository(t)
	ctx := context.Background()
	c, _ := repo.CreateChannel(ctx, channelInput("a", nil))
	if _, err := repo.Enqueue(ctx, notificationFor("evt-1"), []corenotification.Channel{c}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Enqueue(ctx, notificationFor("evt-2"), []corenotification.Channel{c}); err != nil {
		t.Fatal(err)
	}
	page, _ := repo.ListDeliveries(ctx, corenotification.DeliveryFilter{})

	sendErr := `Post "https://prod.example.invalid/workflows/abc?api-version=1&sig=SIGSECRET123": dial tcp: password=hunter2 ` +
		strings.Repeat("x", 2000)
	next := time.Now().Add(time.Minute)
	if err := repo.MarkRetry(ctx, page.Deliveries[0].ID, 1, next, sendErr); err != nil {
		t.Fatalf("MarkRetry: %v", err)
	}
	if err := repo.MarkExhausted(ctx, page.Deliveries[1].ID, 5, sendErr); err != nil {
		t.Fatalf("MarkExhausted: %v", err)
	}

	after, _ := repo.ListDeliveries(ctx, corenotification.DeliveryFilter{})
	for _, d := range after.Deliveries {
		if strings.Contains(d.LastError, "SIGSECRET123") || strings.Contains(d.LastError, "hunter2") {
			t.Errorf("last_error leaks a secret: %q", d.LastError[:120])
		}
		if n := len([]rune(d.LastError)); n != notification.MaxLastErrorLen {
			t.Errorf("last_error is %d characters, want %d", n, notification.MaxLastErrorLen)
		}
		if !strings.Contains(d.LastError, "https://prod.example.invalid/workflows/abc?[REDACTED]") {
			t.Errorf("last_error lost its useful prefix: %q", d.LastError[:120])
		}
		switch d.Attempts {
		case 1:
			if d.Status != corenotification.StatusPending || !d.NextAttemptAt.After(time.Now()) {
				t.Errorf("retried delivery = %+v", d)
			}
		case 5:
			if d.Status != corenotification.StatusFailed {
				t.Errorf("exhausted delivery status = %s", d.Status)
			}
		default:
			t.Errorf("unexpected attempts %d", d.Attempts)
		}
	}
}

// Delivered rows get 30 days; failed rows get 90 — a failure is exactly
// what an operator still wants to see weeks after a success has stopped
// mattering. "recent-failed" (40 days) is the case that exercises that
// difference: under a single 30-day window for both statuses it would be
// deleted; it must survive here.
func TestSweepDeliveriesAppliesRetention(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()
	c, _ := repo.CreateChannel(ctx, channelInput("a", nil))
	for _, evt := range []string{
		"old-delivered", "recent-delivered", "old-failed", "recent-failed", "stale-pending", "fresh-pending",
	} {
		if _, err := repo.Enqueue(ctx, notificationFor(evt), []corenotification.Channel{c}); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`UPDATE notification_deliveries SET status = 'delivered', created_at = now() - interval '31 days' WHERE event_id = 'old-delivered'`,
		`UPDATE notification_deliveries SET status = 'delivered', created_at = now() - interval '29 days' WHERE event_id = 'recent-delivered'`,
		`UPDATE notification_deliveries SET status = 'failed', created_at = now() - interval '91 days' WHERE event_id = 'old-failed'`,
		`UPDATE notification_deliveries SET status = 'failed', created_at = now() - interval '40 days' WHERE event_id = 'recent-failed'`,
		`UPDATE notification_deliveries SET created_at = now() - interval '25 hours' WHERE event_id = 'stale-pending'`,
		`UPDATE notification_deliveries SET created_at = now() - interval '1 hour' WHERE event_id = 'fresh-pending'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}

	res, err := repo.SweepDeliveries(ctx, time.Now())
	if err != nil {
		t.Fatalf("SweepDeliveries: %v", err)
	}
	if res.Deleted != 2 || res.Expired != 1 {
		t.Errorf("sweep = %+v, want 2 deleted, 1 expired", res)
	}

	got := map[string][2]string{}
	rows, err := pool.Query(ctx, `SELECT event_id, status, last_error FROM notification_deliveries`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var evt, status, lastErr string
		if err := rows.Scan(&evt, &status, &lastErr); err != nil {
			t.Fatal(err)
		}
		got[evt] = [2]string{status, lastErr}
	}
	want := map[string][2]string{
		"recent-delivered": {"delivered", ""},
		"recent-failed":    {"failed", ""},
		"stale-pending":    {"failed", "expired"},
		"fresh-pending":    {"pending", ""},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("after sweep = %v, want %v", got, want)
	}
}

// Deleting a channel removes its deliveries with it.
func TestDeleteChannelCascadesDeliveries(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()
	c, _ := repo.CreateChannel(ctx, channelInput("a", nil))
	if _, err := repo.Enqueue(ctx, notificationFor("evt-1"), []corenotification.Channel{c}); err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteChannel(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	var rows int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM notification_deliveries`).Scan(&rows)
	if rows != 0 {
		t.Errorf("deliveries after deleting the channel = %d", rows)
	}
	if err := repo.DeleteChannel(ctx, c.ID); errs.CodeOf(err) != errs.CodeNotFound {
		t.Errorf("deleting again: %v, want not_found", err)
	}
}
