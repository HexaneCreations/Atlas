package fleet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/libp2p/go-libp2p/core/peer"
	"golang.org/x/time/rate"
)

// InstallCodePrefix marks an install code, the same way [TokenPrefix] marks
// an enrollment token.
const InstallCodePrefix = "atlas_install_"

// Install code lifetime bounds. The database enforces the maximum as well
// (migrations/0023_install_codes.sql).
const (
	DefaultInstallCodeTTL = 30 * time.Minute
	MaxInstallCodeTTL     = 60 * time.Minute
)

// Install code statuses, derived from the row's timestamps.
const (
	InstallCodePending  = "pending"
	InstallCodeRedeemed = "redeemed"
	InstallCodeExpired  = "expired"
	InstallCodeRevoked  = "revoked"
)

var (
	installCodeRE = regexp.MustCompile(`^` + InstallCodePrefix + `[0-9a-f]{64}$`)
	nodeIDRE      = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// ErrInstallCodeRejected is the single answer to every failed redemption:
// malformed, unknown, expired, revoked, already spent, a bad Peer ID, or a
// Peer ID or node that already exists. One error, so the response never
// tells a caller which of those it was.
var ErrInstallCodeRejected = errs.New(errs.CodeUnauthenticated, "the install code was not accepted")

// GeneratedInstallCode is a new code; Plaintext is shown once and never stored.
type GeneratedInstallCode struct {
	Plaintext string
	Hash      string
}

// NewInstallCode generates a 256-bit install code.
func NewInstallCode() (GeneratedInstallCode, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return GeneratedInstallCode{}, errs.Wrap(err, errs.CodeInternal, "could not generate install code entropy").
			WithOp("fleet.NewInstallCode")
	}
	plaintext := InstallCodePrefix + hex.EncodeToString(buf)
	return GeneratedInstallCode{Plaintext: plaintext, Hash: HashToken(plaintext)}, nil
}

// ValidPeerID reports whether s is a libp2p Peer ID a real keypair could
// have: it must decode, and either embed a public key that parses (an
// identity-hashed id such as Ed25519's 12D3KooW…) or be a full 32-byte
// sha2-256 multihash (Qm…). peer.Decode alone accepts a truncated identity
// id such as "12D3KooWnope", which no key can ever present.
func ValidPeerID(s string) bool {
	id, err := peer.Decode(s)
	if s == "" || err != nil {
		return false
	}
	if _, err := id.ExtractPublicKey(); err == nil {
		return true
	}
	b := []byte(id)
	return len(b) == 34 && b[0] == 0x12 && b[1] == 0x20
}

// ValidInstallCodeFormat reports whether s has the exact shape of an install code.
func ValidInstallCodeFormat(s string) bool { return installCodeRE.MatchString(s) }

// NewNodeID generates a node id: 128 random bits as 32 lowercase hex.
func NewNodeID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", errs.Wrap(err, errs.CodeInternal, "could not generate a node id").WithOp("fleet.NewNodeID")
	}
	return hex.EncodeToString(buf), nil
}

// InstallCodeSpec describes an install code to create.
type InstallCodeSpec struct {
	Environment string
	// TTLMinutes is optional; nil means [DefaultInstallCodeTTL].
	TTLMinutes *int
	CreatedBy  string
}

// TTL validates the spec and returns its lifetime.
func (s InstallCodeSpec) TTL() (time.Duration, error) {
	const op = "fleet.InstallCodeSpec.TTL"
	if !environmentRE.MatchString(s.Environment) {
		return 0, errs.New(errs.CodeInvalidArgument,
			"environment must be 1-64 characters of letters, digits, '.', '_' or '-'").WithOp(op)
	}
	if s.CreatedBy == "" {
		return 0, errs.New(errs.CodeInvalidArgument, "an install code must record who created it").WithOp(op)
	}
	if s.TTLMinutes == nil {
		return DefaultInstallCodeTTL, nil
	}
	ttl := time.Duration(*s.TTLMinutes) * time.Minute
	if ttl <= 0 || ttl > MaxInstallCodeTTL {
		return 0, errs.New(errs.CodeInvalidArgument, "ttl_minutes must be between 1 and %d",
			int(MaxInstallCodeTTL/time.Minute)).WithOp(op)
	}
	return ttl, nil
}

// InstallCode is a stored install code as an operator sees it. It never
// carries the code itself.
type InstallCode struct {
	ID             string
	NodeID         string
	Environment    string
	CreatedBy      string
	CreatedAt      time.Time
	ExpiresAt      time.Time
	RevokedAt      *time.Time
	RevokedBy      string
	RedeemedAt     *time.Time
	RedeemedPeerID string
	RedeemedIP     string
}

// Status derives the code's state at now.
func (c InstallCode) Status(now time.Time) string {
	switch {
	case c.RedeemedAt != nil:
		return InstallCodeRedeemed
	case c.RevokedAt != nil:
		return InstallCodeRevoked
	case !now.Before(c.ExpiresAt):
		return InstallCodeExpired
	default:
		return InstallCodePending
	}
}

// Redemption is what a successful redeem binds: the Peer ID now authorized
// to act as NodeID in Environment.
type Redemption struct {
	CodeID      string
	NodeID      string
	Environment string
}

// InstallCodeStore persists install codes. Implemented by
// internal/storage/fleet.Repository.
type InstallCodeStore interface {
	// CreateInstallCode stores a code with a freshly generated node id that
	// exists nowhere yet (nodes, agent_peers, install_codes).
	CreateInstallCode(ctx context.Context, codeHash string, spec InstallCodeSpec, now time.Time) (InstallCode, error)
	ListInstallCodes(ctx context.Context) ([]InstallCode, error)
	// RevokeInstallCode revokes a pending code. A code that is not pending
	// (or does not exist) is a not-found or failed-precondition error.
	RevokeInstallCode(ctx context.Context, id, revokedBy string, now time.Time) (InstallCode, error)
	// RedeemInstallCode spends a pending code and authorizes peerID for its
	// node, atomically. Any refusal is [ErrInstallCodeRejected] and writes
	// nothing.
	RedeemInstallCode(ctx context.Context, codeHash, peerID string, sourceIP net.IP, now time.Time) (Redemption, error)
}

// Character allowlists the install command's values must satisfy. They are
// the regexes of packaging/atlas-agent/install.sh's validate() calls; a test
// reads that file and checks generated commands against its own copies.
var (
	urlRE         = regexp.MustCompile(`^https://([A-Za-z0-9.-]+|\[[0-9A-Fa-f:.]+\])(:[0-9]{1,5})?(/[A-Za-z0-9._~/-]*)?$`)
	multiaddrRE   = regexp.MustCompile(`^/[A-Za-z0-9./:_-]+$`)
	environmentRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	tokenRE       = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)
	versionRE     = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`)
)

// InstallScriptRepo is the public repository the command downloads
// install.sh from, pinned to the release tag. It matches install.sh's own
// RELEASE_REPO.
const InstallScriptRepo = "HexaneCreations/Atlas"

// InstallTarget is the server-side information an install command needs.
type InstallTarget struct {
	// Version is the agent release to install: this server's own build
	// version, which must be a release tag.
	Version string
	// ControlPlaneURL is the public https base URL (no trailing slash).
	ControlPlaneURL string
	// ServerAddr is the full libp2p multiaddr agents dial, ending in
	// /p2p/<server peer id>.
	ServerAddr string
}

// Validate checks every value against the installer's allowlists.
func (t InstallTarget) Validate() error {
	const op = "fleet.InstallTarget.Validate"
	switch {
	case !versionRE.MatchString(t.Version):
		return errs.New(errs.CodeFailedPrecondition,
			"this server's version %q is not a release tag, so there is no matching agent release to install", t.Version).WithOp(op)
	case !urlRE.MatchString(t.ControlPlaneURL):
		return errs.New(errs.CodeFailedPrecondition,
			"the public URL %q is not an https URL the installer accepts", t.ControlPlaneURL).WithOp(op)
	case !multiaddrRE.MatchString(t.ServerAddr) || !strings.Contains(t.ServerAddr, "/p2p/"):
		return errs.New(errs.CodeFailedPrecondition,
			"the libp2p server address %q is not a multiaddr with a peer id", t.ServerAddr).WithOp(op)
	}
	return nil
}

// InstallCommand renders the one-line install command for code. Every value
// is checked against the installer's allowlists first and single-quoted; the
// allowlists exclude the quote character, so quoting cannot be broken.
func InstallCommand(t InstallTarget, code string) (string, error) {
	if err := t.Validate(); err != nil {
		return "", err
	}
	if !ValidInstallCodeFormat(code) || !tokenRE.MatchString(code) {
		return "", errs.New(errs.CodeInternal, "generated install code has an unexpected shape").WithOp("fleet.InstallCommand")
	}
	script := "https://raw.githubusercontent.com/" + InstallScriptRepo + "/" + t.Version + "/packaging/atlas-agent/install.sh"
	return "d=\"$(mktemp -d)\" && " +
		"curl --proto '=https' --tlsv1.2 -fsSL -o \"$d/install.sh\" '" + script + "' && " +
		"sudo bash \"$d/install.sh\"" +
		" --version '" + t.Version + "'" +
		" --transport libp2p" +
		" --control-plane-url '" + t.ControlPlaneURL + "'" +
		" --server-addr '" + t.ServerAddr + "'" +
		" --install-code '" + code + "'", nil
}

// Redeem rate-limit defaults: per source address, and across all sources.
const (
	RedeemPerIPPerMinute  = 10
	RedeemGlobalPerMinute = 60
)

// RedeemLimiter bounds install-code redemption attempts per source IP and
// globally. Both budgets are charged on every attempt, before any check.
type RedeemLimiter struct {
	perIPLimit rate.Limit
	perIPBurst int
	global     *rate.Limiter

	mu      sync.Mutex
	byIP    map[string]*ipBucket
	lastGC  time.Time
	nowFunc func() time.Time
}

type ipBucket struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// NewRedeemLimiter builds a limiter allowing perIP attempts per minute from
// one address and global attempts per minute in total, each with a burst of
// the same size.
func NewRedeemLimiter(perIP, global int) *RedeemLimiter {
	return &RedeemLimiter{
		perIPLimit: rate.Limit(float64(perIP) / 60), perIPBurst: perIP,
		global: rate.NewLimiter(rate.Limit(float64(global)/60), global),
		byIP:   map[string]*ipBucket{}, nowFunc: time.Now,
	}
}

// Allow reports whether an attempt from ip may proceed.
func (l *RedeemLimiter) Allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.nowFunc()
	b, ok := l.byIP[ip]
	if !ok {
		b = &ipBucket{limiter: rate.NewLimiter(l.perIPLimit, l.perIPBurst)}
		l.byIP[ip] = b
	}
	b.lastSeen = now
	if now.Sub(l.lastGC) > 30*time.Minute {
		for k, v := range l.byIP {
			if now.Sub(v.lastSeen) > 30*time.Minute {
				delete(l.byIP, k)
			}
		}
		l.lastGC = now
	}

	ipOK := b.limiter.AllowN(now, 1)
	globalOK := l.global.AllowN(now, 1)
	return ipOK && globalOK
}
