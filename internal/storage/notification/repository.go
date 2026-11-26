// Package notification is the PostgreSQL store for notification channels,
// rules, evaluator state and deliveries (see
// internal/core/notification and migrations/0024_notification_schema.sql).
package notification

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	corenotification "github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/id"
	"github.com/hexane/atlas/internal/platform/secretbox"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository reads and writes the notification tables. Channel secrets are
// sealed with key, bound to the channel id; a nil key refuses every secret
// operation rather than storing plaintext.
type Repository struct {
	pool *pgxpool.Pool
	key  *secretbox.Key
}

// NewRepository builds a repository over a started pool.
func NewRepository(pool *pgxpool.Pool, key *secretbox.Key) *Repository {
	return &Repository{pool: pool, key: key}
}

const channelColumns = `id, name, type, enabled, min_severity, config, secret_ciphertext IS NOT NULL, created_at, updated_at`

func scanChannel(row pgx.Row) (corenotification.Channel, error) {
	var c corenotification.Channel
	var typ, minSeverity string
	var config []byte
	if err := row.Scan(&c.ID, &c.Name, &typ, &c.Enabled, &minSeverity, &config,
		&c.SecretConfigured, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return corenotification.Channel{}, err
	}
	c.Type = corenotification.ChannelType(typ)
	c.MinSeverity = corenotification.Severity(minSeverity)
	if err := json.Unmarshal(config, &c.Config); err != nil {
		return corenotification.Channel{}, fmt.Errorf("decode channel config: %w", err)
	}
	return c, nil
}

func encodeConfig(config map[string]any) ([]byte, error) {
	if config == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(config)
}

// seal encrypts secret for channelID. The error never carries the secret.
func (r *Repository) seal(op, channelID, secret string) (secretbox.Sealed, error) {
	if r.key == nil {
		return secretbox.Sealed{}, errs.New(errs.CodeUnavailable,
			"notifications are disabled: no encryption key is configured").WithOp(op)
	}
	sealed, err := r.key.Seal([]byte(secret), []byte(channelID))
	if err != nil {
		return secretbox.Sealed{}, errs.New(errs.CodeInternal, "could not encrypt the channel secret").WithOp(op)
	}
	return sealed, nil
}

// pgCode is err's PostgreSQL SQLSTATE, or "".
func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func isUniqueViolation(err error) bool { return pgCode(err) == "23505" }

func channelWriteError(err error, op, verb string) error {
	if isUniqueViolation(err) {
		return errs.New(errs.CodeAlreadyExists, "a notification channel with this name already exists").WithOp(op)
	}
	return errs.Wrap(err, errs.CodeUnavailable, "could not %s the notification channel", verb).WithOp(op)
}

// ListChannels returns every channel, newest first. Secrets are reported
// only as configured or not.
func (r *Repository) ListChannels(ctx context.Context) ([]corenotification.Channel, error) {
	const op = "notification.Repository.ListChannels"

	rows, err := r.pool.Query(ctx, `SELECT `+channelColumns+` FROM notification_channels ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list notification channels").WithOp(op)
	}
	defer rows.Close()

	channels := []corenotification.Channel{}
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read a notification channel").WithOp(op)
		}
		channels = append(channels, c)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list notification channels").WithOp(op)
	}
	return channels, nil
}

// GetChannel returns one channel by id, without its secret.
func (r *Repository) GetChannel(ctx context.Context, channelID string) (corenotification.Channel, error) {
	const op = "notification.Repository.GetChannel"

	c, err := scanChannel(r.pool.QueryRow(ctx, `SELECT `+channelColumns+` FROM notification_channels WHERE id = $1`, channelID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return corenotification.Channel{}, errs.New(errs.CodeNotFound, "notification channel not found").WithOp(op).WithDetail("id", channelID)
		}
		return corenotification.Channel{}, errs.Wrap(err, errs.CodeUnavailable, "could not read the notification channel").WithOp(op)
	}
	return c, nil
}

// CreateChannel persists a new channel, assigning its id and sealing its
// secret, if any, to that id.
func (r *Repository) CreateChannel(ctx context.Context, in corenotification.ChannelInput) (corenotification.Channel, error) {
	const op = "notification.Repository.CreateChannel"
	if err := in.Validate(); err != nil {
		return corenotification.Channel{}, err
	}
	config, err := encodeConfig(in.Config)
	if err != nil {
		return corenotification.Channel{}, errs.Wrap(err, errs.CodeInvalidArgument, "the channel config is not valid JSON").WithOp(op)
	}

	channelID := id.New()
	var ciphertext []byte
	var keyID *string
	if in.Secret != nil {
		sealed, err := r.seal(op, channelID, *in.Secret)
		if err != nil {
			return corenotification.Channel{}, err
		}
		ciphertext, keyID = sealed.Ciphertext, &sealed.KeyID
	}

	const q = `
		INSERT INTO notification_channels (id, name, type, enabled, min_severity, config, secret_ciphertext, secret_key_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING ` + channelColumns
	c, err := scanChannel(r.pool.QueryRow(ctx, q, channelID, in.Name, string(in.Type), in.Enabled,
		string(in.MinSeverity), config, ciphertext, keyID))
	if err != nil {
		return corenotification.Channel{}, channelWriteError(err, op, "create")
	}
	return c, nil
}

// UpdateChannel replaces a channel's settings. A nil in.Secret keeps the
// stored secret; a non-nil one replaces it, sealed to the same id.
func (r *Repository) UpdateChannel(ctx context.Context, channelID string, in corenotification.ChannelInput) (corenotification.Channel, error) {
	const op = "notification.Repository.UpdateChannel"
	if err := in.Validate(); err != nil {
		return corenotification.Channel{}, err
	}
	config, err := encodeConfig(in.Config)
	if err != nil {
		return corenotification.Channel{}, errs.Wrap(err, errs.CodeInvalidArgument, "the channel config is not valid JSON").WithOp(op)
	}

	replaceSecret := in.Secret != nil
	var ciphertext []byte
	var keyID *string
	if replaceSecret {
		sealed, err := r.seal(op, channelID, *in.Secret)
		if err != nil {
			return corenotification.Channel{}, err
		}
		ciphertext, keyID = sealed.Ciphertext, &sealed.KeyID
	}

	const q = `
		UPDATE notification_channels SET
			name = $2, type = $3, enabled = $4, min_severity = $5, config = $6,
			secret_ciphertext = CASE WHEN $7 THEN $8 ELSE secret_ciphertext END,
			secret_key_id     = CASE WHEN $7 THEN $9 ELSE secret_key_id END,
			updated_at = now()
		WHERE id = $1
		RETURNING ` + channelColumns
	c, err := scanChannel(r.pool.QueryRow(ctx, q, channelID, in.Name, string(in.Type), in.Enabled,
		string(in.MinSeverity), config, replaceSecret, ciphertext, keyID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return corenotification.Channel{}, errs.New(errs.CodeNotFound, "notification channel not found").WithOp(op).WithDetail("id", channelID)
		}
		return corenotification.Channel{}, channelWriteError(err, op, "update")
	}
	return c, nil
}

// DeleteChannel removes a channel. Its deliveries cascade with it.
func (r *Repository) DeleteChannel(ctx context.Context, channelID string) error {
	const op = "notification.Repository.DeleteChannel"

	tag, err := r.pool.Exec(ctx, `DELETE FROM notification_channels WHERE id = $1`, channelID)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not delete the notification channel").WithOp(op)
	}
	if tag.RowsAffected() == 0 {
		return errs.New(errs.CodeNotFound, "notification channel not found").WithOp(op).WithDetail("id", channelID)
	}
	return nil
}

// ChannelSecret returns a channel's decrypted secret. It is for sending, and
// for masking or completing the secret in internal/core/notification; nothing
// serves it to a reader. A secret sealed under a
// different key, or altered, is [corenotification.ErrSecretUnreadable]
// (also matching secretbox.ErrKeyMismatch or ErrDecrypt); no error carries
// secret material.
func (r *Repository) ChannelSecret(ctx context.Context, channelID string) (string, error) {
	const op = "notification.Repository.ChannelSecret"
	if r.key == nil {
		return "", errs.New(errs.CodeUnavailable, "notifications are disabled: no encryption key is configured").WithOp(op)
	}

	var ciphertext []byte
	var keyID *string
	err := r.pool.QueryRow(ctx,
		`SELECT secret_ciphertext, secret_key_id FROM notification_channels WHERE id = $1`, channelID).Scan(&ciphertext, &keyID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", errs.New(errs.CodeNotFound, "notification channel not found").WithOp(op).WithDetail("id", channelID)
		}
		return "", errs.Wrap(err, errs.CodeUnavailable, "could not read the notification channel secret").WithOp(op)
	}
	if ciphertext == nil || keyID == nil {
		return "", errs.New(errs.CodeFailedPrecondition, "the notification channel has no secret configured").
			WithOp(op).WithDetail("id", channelID)
	}

	plaintext, err := r.key.Open(secretbox.Sealed{KeyID: *keyID, Ciphertext: ciphertext}, []byte(channelID))
	if err != nil {
		return "", errs.Wrap(fmt.Errorf("%w: %w", corenotification.ErrSecretUnreadable, err),
			errs.CodeFailedPrecondition, "could not decrypt the channel secret").
			WithOp(op).WithDetail("id", channelID).WithDetail("stored_key_id", *keyID)
	}
	return string(plaintext), nil
}
