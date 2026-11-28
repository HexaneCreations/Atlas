// Package monitor probes websites from atlas-server: HTTP(S) monitors, their
// up/down state machine, the scheduler that runs checks, and the guard that
// keeps a configured URL from reaching anything it must not.
package monitor

import (
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/safehttp"
)

// Method is the HTTP method a check sends.
type Method string

const (
	MethodGet  Method = "GET"
	MethodHead Method = "HEAD"
	MethodPost Method = "POST"
)

// AuthType is how a check authenticates.
type AuthType string

const (
	AuthNone   AuthType = "none"
	AuthBasic  AuthType = "basic"
	AuthBearer AuthType = "bearer"
)

// BodyEncoding sets the Content-Type a POST body is sent with.
type BodyEncoding string

const (
	BodyJSON BodyEncoding = "json"
	BodyForm BodyEncoding = "form"
	BodyText BodyEncoding = "text"
)

// Status is a monitor's state, and also a heartbeat's.
type Status string

const (
	// StatusUnknown: not checked yet; as a heartbeat, the probe itself had
	// no connectivity, so the check says nothing about the site.
	StatusUnknown Status = "unknown"
	StatusUp      Status = "up"
	// StatusPending: failing, but not yet for more than Retries checks.
	StatusPending Status = "pending"
	StatusDown    Status = "down"
)

// Failure is why a check failed. It is the only failure detail stored:
// never a response body, never error text (which can carry the URL).
type Failure string

const (
	FailureNone       Failure = ""
	FailureDNS        Failure = "dns"
	FailureConnect    Failure = "connect"
	FailureTLS        Failure = "tls"
	FailureTimeout    Failure = "timeout"
	FailureHTTPStatus Failure = "http_status"
	FailureKeyword    Failure = "keyword"
	FailureRedirect   Failure = "redirect"
	// FailureBlocked: the URL resolved to an address the guard refuses.
	FailureBlocked Failure = "blocked"
)

// network reports whether f says nothing about the site itself when the
// probe has lost connectivity.
func (f Failure) network() bool {
	return f == FailureDNS || f == FailureConnect || f == FailureTimeout
}

// Limits on what a monitor may be configured with.
const (
	MinIntervalSeconds = 20
	MaxIntervalSeconds = 86400
	MaxTimeoutSeconds  = 120
	MaxRedirects       = 10
	MaxRetries         = 10
	MaxRecovery        = 10
	MaxURLLength       = 2048
	MaxNameLength      = 100
	MaxDescription     = 1000
	MaxKeywordLength   = 500
	MaxTags            = 20
	MaxTagLength       = 50
	MaxStatusRanges    = 20
)

// Monitor is a monitor's stored configuration, without its secret.
type Monitor struct {
	ID                   string
	Name                 string
	URL                  string
	Method               Method
	IntervalSeconds      int
	Retries              int
	RetryIntervalSeconds int
	TimeoutSeconds       int
	MaxRedirects         int
	AcceptedStatus       []string
	BodyEncoding         BodyEncoding
	Keyword              string
	RecoverySuccesses    int
	CertNotify           bool
	CertWarnDays         int
	CertCritDays         int
	GroupID              string
	Description          string
	Tags                 []string
	Active               bool
	AlertsMutedUntil     *time.Time
	AuthType             AuthType
	// HeaderNames are the custom header names; their values are secret.
	HeaderNames      []string
	SecretConfigured bool
	// ChannelIDs are the notification channels the monitor alerts. Read
	// back on every view; on create nil selects the channels flagged
	// monitor_default, on update nil keeps the selection.
	ChannelIDs []string
	CreatedAt  time.Time
	CreatedBy  string
	UpdatedAt  time.Time
	UpdatedBy  string
}

// Defaults fills every unset field of a new monitor's configuration.
func Defaults() Monitor {
	return Monitor{
		Method: MethodGet, IntervalSeconds: 60, Retries: 3, RetryIntervalSeconds: 60, TimeoutSeconds: 30,
		MaxRedirects: 10, AcceptedStatus: []string{"200-299"}, BodyEncoding: BodyJSON, RecoverySuccesses: 1,
		CertNotify: true, CertWarnDays: 14, CertCritDays: 3, Active: true, AuthType: AuthNone,
		Tags: []string{}, HeaderNames: []string{},
	}
}

func invalid(field, format string, args ...any) error {
	return errs.New(errs.CodeInvalidArgument, format, args...).WithOp("monitor.Validate").WithDetail("field", field)
}

// Validate checks m's configuration. allowPrivate is the server's
// monitoring.allow_private_targets setting. It resolves nothing: a name
// that resolves to a refused address is refused when it is dialed.
func Validate(m Monitor, allowPrivate bool) error {
	switch {
	case strings.TrimSpace(m.Name) == "" || len(m.Name) > MaxNameLength:
		return invalid("name", "name must be 1 to %d characters", MaxNameLength)
	case !slices.Contains([]Method{MethodGet, MethodHead, MethodPost}, m.Method):
		return invalid("method", "method must be GET, HEAD or POST")
	case m.IntervalSeconds < MinIntervalSeconds || m.IntervalSeconds > MaxIntervalSeconds:
		return invalid("interval_seconds", "the interval must be between %d and %d seconds", MinIntervalSeconds, MaxIntervalSeconds)
	case m.RetryIntervalSeconds < MinIntervalSeconds || m.RetryIntervalSeconds > MaxIntervalSeconds:
		return invalid("retry_interval_seconds", "the retry interval must be between %d and %d seconds", MinIntervalSeconds, MaxIntervalSeconds)
	case m.TimeoutSeconds < 1 || m.TimeoutSeconds > MaxTimeoutSeconds:
		return invalid("timeout_seconds", "the timeout must be between 1 and %d seconds", MaxTimeoutSeconds)
	case m.TimeoutSeconds >= m.IntervalSeconds:
		return invalid("timeout_seconds", "the timeout must be shorter than the interval")
	case m.Retries < 0 || m.Retries > MaxRetries:
		return invalid("retries", "retries must be between 0 and %d", MaxRetries)
	case m.MaxRedirects < 0 || m.MaxRedirects > MaxRedirects:
		return invalid("max_redirects", "max redirects must be between 0 and %d", MaxRedirects)
	case m.RecoverySuccesses < 1 || m.RecoverySuccesses > MaxRecovery:
		return invalid("recovery_successes", "recovery successes must be between 1 and %d", MaxRecovery)
	case !slices.Contains([]BodyEncoding{BodyJSON, BodyForm, BodyText}, m.BodyEncoding):
		return invalid("body_encoding", "body encoding must be json, form or text")
	case len(m.Keyword) > MaxKeywordLength:
		return invalid("keyword", "the keyword must be at most %d characters", MaxKeywordLength)
	case m.Keyword != "" && m.Method == MethodHead:
		return invalid("keyword", "a HEAD request has no body to search for a keyword")
	case m.CertWarnDays < 1 || m.CertWarnDays > 365 || m.CertCritDays < 0 || m.CertCritDays >= m.CertWarnDays:
		return invalid("cert_crit_days", "certificate days must satisfy 0 <= critical < warning <= 365")
	case len(m.Description) > MaxDescription:
		return invalid("description", "the description must be at most %d characters", MaxDescription)
	case !slices.Contains([]AuthType{AuthNone, AuthBasic, AuthBearer}, m.AuthType):
		return invalid("auth_type", "authentication must be none, basic or bearer")
	}
	if _, err := ParseStatusCodes(m.AcceptedStatus); err != nil {
		return err
	}
	if err := validateTags(m.Tags); err != nil {
		return err
	}
	return ValidateURL(m.URL, allowPrivate)
}

func validateTags(tags []string) error {
	if len(tags) > MaxTags {
		return invalid("tags", "at most %d tags", MaxTags)
	}
	seen := map[string]bool{}
	for _, t := range tags {
		if strings.TrimSpace(t) != t || t == "" || len(t) > MaxTagLength || strings.ContainsFunc(t, unicode.IsControl) {
			return invalid("tags", "a tag must be 1 to %d characters with no surrounding spaces", MaxTagLength)
		}
		if seen[strings.ToLower(t)] {
			return invalid("tags", "tag %q is listed twice", t)
		}
		seen[strings.ToLower(t)] = true
	}
	return nil
}

// ValidateURL accepts an absolute http(s) URL with a host and no userinfo.
// Without allowPrivate it also refuses a host that is visibly not public:
// localhost, a non-public IP literal, or a numeric form a resolver may read
// as an IPv4 address (127.1, 0x7f000001). Errors never echo the URL.
func ValidateURL(raw string, allowPrivate bool) error {
	bad := func(msg string) error { return invalid("url", "%s", msg) }
	if raw == "" || len(raw) > MaxURLLength {
		return bad("the URL must be 1 to 2048 characters")
	}
	if strings.ContainsFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return bad("the URL must not contain spaces or control characters")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return bad("the URL is not valid")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return bad("the URL must start with http:// or https://")
	}
	if u.User != nil {
		return bad("the URL must not contain a user name or password; use the authentication settings")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return bad("the URL must name a host")
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return bad("the URL's port is not valid")
		}
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if !safehttp.Allowed(net.IP(addr.WithZone("").AsSlice()), allowPrivate) {
			return bad("the URL's address is not allowed")
		}
		return nil
	}
	last := host[strings.LastIndex(host, ".")+1:]
	if strings.HasPrefix(last, "0x") || strings.Trim(last, "0123456789") == "" {
		return bad("the URL's host looks like a numeric address; write the address in dotted form")
	}
	if !allowPrivate && (host == "localhost" || strings.HasSuffix(host, ".localhost")) {
		return bad("the URL's address is not allowed")
	}
	return nil
}

// Host is rawURL's host name, the only part of the URL logs, audit entries
// and notifications show.
func Host(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// RedactedURL drops the query and fragment, where a token is most often
// put, for readers who may see status but not secrets.
func RedactedURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
}

// Group is a named group of monitors.
type Group struct {
	ID        string
	Name      string
	Position  int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ValidateGroup checks a group's name.
func ValidateGroup(g Group) error {
	if strings.TrimSpace(g.Name) == "" || len(g.Name) > MaxNameLength || strings.ContainsFunc(g.Name, unicode.IsControl) {
		return invalid("name", "a group name must be 1 to %d characters", MaxNameLength)
	}
	return nil
}
