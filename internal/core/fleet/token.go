// Package fleet is the domain logic of machine enrollment: who may obtain a
// node certificate, under what bounds, and the rule that stops a stolen
// credential from taking over an identity that already exists.
//
// It depends on [pki] to sign certificates and on two narrow storage
// interfaces it defines itself ([TokenStore], [CredentialStore]), not on
// Postgres — the same separation [inventory] and [metric] use, and for the
// same reason: the rules here (bounded tokens, re-enrollment refusal, denylist
// checks) are what an incident review reads to understand what enrollment
// actually enforces, and they should be readable without a database running.
package fleet

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
)

// tokenBytes is the amount of entropy in a generated token. 256 bits, so
// brute-forcing it is not a meaningfully weaker attack than brute-forcing the
// TLS session that redeems it.
const tokenBytes = 32

// TokenPrefix marks a string as an Atlas enrollment token, the way GitHub's
// "ghp_" or Stripe's "sk_" prefixes do — it lets a leaked-secret scanner
// recognise one on sight, and lets an operator tell an enrollment token from
// any other string in a log line.
const TokenPrefix = "atlas_enroll_"

// GeneratedToken is a freshly created token, returned once.
//
// Plaintext is shown to the operator exactly once, at creation, and is never
// persisted — only [GeneratedToken.Hash] is stored. There is no "reveal
// token" API and there must never be one; that is precisely the shared-secret
// mistake [docs/architecture/agent-design.md] §4 rejected.
type GeneratedToken struct {
	Plaintext string
	Hash      string
}

// NewToken generates a fresh enrollment token.
func NewToken() (GeneratedToken, error) {
	const op = "fleet.NewToken"

	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return GeneratedToken{}, errs.Wrap(err, errs.CodeInternal, "could not generate token entropy").WithOp(op)
	}
	plaintext := TokenPrefix + hex.EncodeToString(buf)
	return GeneratedToken{Plaintext: plaintext, Hash: HashToken(plaintext)}, nil
}

// HashToken returns the stored form of a token presented by an agent.
//
// Plain SHA-256 rather than a slow password hash (bcrypt/argon2): the input
// space is 256 bits of real entropy, not a human-chosen password, so there is
// nothing for a slow hash to protect against that the entropy does not
// already provide, and enrollment is on a path an operator wants to be fast.
func HashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// TokenGrant is a redeemed token's bounds, as recorded when it was created.
// Returned by [TokenStore.Redeem] on success.
type TokenGrant struct {
	Environment   string
	AllowReenroll bool
}

// TokenSpec describes a token to create.
type TokenSpec struct {
	Label string
	// Environment tags every node enrolled with this token. Required: an
	// untagged environment is a fact Atlas will not invent (see
	// config.Node.Environment's doc), and that applies to fleet-provisioned
	// nodes the same as to a manually configured one.
	Environment string
	// AllowedCIDR restricts which source addresses may redeem the token.
	// Empty is rejected by [TokenSpec.Validate] — an operator must set an
	// unrestricted range explicitly (e.g. "0.0.0.0/0") rather than get it by
	// omission.
	AllowedCIDR string
	MaxUses     int
	TTL         time.Duration
	// AllowReenroll grants this token the right to enroll a node id that
	// already holds a live certificate. See docs/architecture/agent-design.md
	// sec 4 — off by default, and should stay off for anything but a
	// deliberate re-provisioning token.
	AllowReenroll bool
}

// Validate reports whether the spec is usable.
func (s TokenSpec) Validate() error {
	const op = "fleet.TokenSpec.Validate"
	switch {
	case s.Environment == "":
		return errs.New(errs.CodeInvalidArgument, "a token must name the environment it enrolls nodes into").WithOp(op)
	case s.AllowedCIDR == "":
		return errs.New(errs.CodeInvalidArgument,
			"a token must state which network may redeem it; use 0.0.0.0/0 to allow any").WithOp(op)
	case s.MaxUses <= 0:
		return errs.New(errs.CodeInvalidArgument, "max uses must be positive").WithOp(op)
	case s.TTL <= 0:
		return errs.New(errs.CodeInvalidArgument, "TTL must be positive").WithOp(op)
	}
	return nil
}

// ErrTokenInvalid is returned by [TokenStore.Redeem] when a token cannot be
// redeemed for any reason — expired, exhausted, revoked, or unknown. The
// reasons are deliberately not distinguished in the type a caller sees: an
// enrollment endpoint that told a caller *why* a token failed would hand an
// attacker a probe for which invalid tokens are merely expired versus which
// never existed.
var ErrTokenInvalid = errs.New(errs.CodeUnauthenticated, "the enrollment token could not be redeemed")

// FormatFailure builds the wrapped, audited form of a token failure. The
// human-readable reason goes in the log and the audit event; the response the
// caller receives is always [ErrTokenInvalid]'s message.
func FormatFailure(op, reason string) error {
	return errs.Wrap(ErrTokenInvalid, errs.CodeUnauthenticated, "the enrollment token could not be redeemed").
		WithOp(op).WithDetail("reason", reason)
}
