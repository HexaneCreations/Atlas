package notification

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/hexane/atlas/internal/platform/errs"
)

// Caps applied to a "teams" or "webhook" channel's non-secret config and
// sealed secret alike. Not mandated by any particular number in the brief
// this implements — chosen generously enough not to constrain a real
// Workflows URL or template, tightly enough to bound what a channel can
// make this process hold in memory or send.
const (
	MaxDestinationURLLength = 2048
	MaxHeaderCount          = 20
	MaxHeaderNameLength     = 128
	MaxHeaderValueLength    = 4096
	MaxTemplateLength       = 16 * 1024
)

// TeamsAllowedHostSuffixes are the only hosts a "teams" channel's URL may
// have. Microsoft Teams "Workflows" (the Power Automate-backed webhook
// that replaced Office 365 Connectors, which Microsoft retired in 2024-25)
// issues URLs under logic.azure.com or api.powerplatform.com;
// webhook.office.com is kept for a tenant still on the older Incoming
// Webhook connector. Matching is by host suffix against a single slice, so
// a documented hostname revision is a one-line change here, not a
// redesign.
//
// Verified only as far as these three hostnames themselves being
// Microsoft-documented Workflows/connector domains: this session had no
// way to fetch Microsoft's current live documentation page to confirm the
// exact URL *path* shape ("/workflows/.../triggers/manual/paths/invoke")
// still matches what Workflows issues today. Flagged per the brief's own
// instruction rather than asserted as verified.
var TeamsAllowedHostSuffixes = []string{
	"logic.azure.com",
	"api.powerplatform.com",
	"webhook.office.com",
}

// headerDenyList are header names a channel's config may never set,
// matched by canonical form. Authorization is deliberately not here: users
// legitimately need it for a destination that requires a bearer or basic
// credential, and withholding it would just push them to smuggle the same
// value into the body instead.
var headerDenyList = map[string]bool{
	"Host":              true,
	"Content-Length":    true,
	"Transfer-Encoding": true,
	"Connection":        true,
	"Cookie":            true,
}

// validateDestinationURL checks a channel's destination URL against the
// rules every HTTP-based channel type shares: https only, within the
// length cap, no userinfo (a credential belongs in the secret or a header,
// not visible in the URL every reader of the channel's config can see),
// and — when allowedHostSuffixes is non-empty — a host ending in one of
// them.
func validateDestinationURL(raw string, allowedHostSuffixes []string, op string) error {
	if raw == "" {
		return errs.New(errs.CodeInvalidArgument, "a destination url is required").WithOp(op).WithDetail("field", "url")
	}
	if len(raw) > MaxDestinationURLLength {
		return errs.New(errs.CodeInvalidArgument, "the destination url exceeds %d characters", MaxDestinationURLLength).WithOp(op).WithDetail("field", "url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errs.New(errs.CodeInvalidArgument, "the destination url is not valid").WithOp(op).WithDetail("field", "url")
	}
	if u.Scheme != "https" {
		return errs.New(errs.CodeInvalidArgument, "the destination url must use https").WithOp(op).WithDetail("field", "url")
	}
	if u.User != nil {
		return errs.New(errs.CodeInvalidArgument, "the destination url must not carry userinfo").WithOp(op).WithDetail("field", "url")
	}
	if strings.ContainsAny(raw, "\r\n") {
		return errs.New(errs.CodeInvalidArgument, "the destination url must not contain a newline").WithOp(op).WithDetail("field", "url")
	}
	if len(allowedHostSuffixes) == 0 {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	for _, suffix := range allowedHostSuffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return nil
		}
	}
	return errs.New(errs.CodeInvalidArgument,
		"the destination host %q is not one of %s", host, strings.Join(allowedHostSuffixes, ", ")).WithOp(op).WithDetail("field", "url")
}

// validateHeaders checks a channel's additional-header config: no more
// than [MaxHeaderCount], each name and value within its length cap, no
// denied header name, no character HTTP does not allow, and so no CR or LF
// in any name or value (which would otherwise let a header value inject a
// second header or split the request).
func validateHeaders(headers map[string]string, op string) error {
	if len(headers) > MaxHeaderCount {
		return errs.New(errs.CodeInvalidArgument, "at most %d headers are allowed, got %d", MaxHeaderCount, len(headers)).WithOp(op).WithDetail("field", "headers")
	}
	for name, value := range headers {
		if name == "" {
			return errs.New(errs.CodeInvalidArgument, "a header name must not be empty").WithOp(op).WithDetail("field", "headers")
		}
		if strings.ContainsAny(name, "\r\n") || strings.ContainsAny(value, "\r\n") {
			return errs.New(errs.CodeInvalidArgument, "header %q must not contain a newline in its name or value", name).WithOp(op).WithDetail("field", "headers")
		}
		if strings.ContainsFunc(name, notHeaderNameChar) || strings.ContainsFunc(value, notHeaderValueChar) {
			return errs.New(errs.CodeInvalidArgument, "header %q has a character HTTP does not allow", name).WithOp(op).WithDetail("field", "headers")
		}
		if len(name) > MaxHeaderNameLength {
			return errs.New(errs.CodeInvalidArgument, "header name %q exceeds %d characters", name, MaxHeaderNameLength).WithOp(op).WithDetail("field", "headers")
		}
		if len(value) > MaxHeaderValueLength {
			return errs.New(errs.CodeInvalidArgument, "header %q value exceeds %d characters", name, MaxHeaderValueLength).WithOp(op).WithDetail("field", "headers")
		}
		if headerDenyList[http.CanonicalHeaderKey(name)] {
			return errs.New(errs.CodeInvalidArgument, "header %q may not be set by channel config", name).WithOp(op).WithDetail("field", "headers")
		}
	}
	return nil
}

// notHeaderNameChar reports whether r may not appear in a header name (an
// RFC 9110 token); net/http refuses to send one that has it.
func notHeaderNameChar(r rune) bool {
	return !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", r))
}

func notHeaderValueChar(r rune) bool { return r < ' ' && r != '\t' || r == 0x7f }

// maskURL reduces a destination URL to its scheme and host, the only part
// safe to show a reader who can see a channel exists but must not see
// where it points in enough detail to use it: the path and query of a
// Teams Workflows or signed webhook URL are themselves the credential.
func maskURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return Placeholder
	}
	return fmt.Sprintf("%s://%s/%s", u.Scheme, u.Host, Placeholder)
}

// maskedHeaderNames returns header names only, sorted, never their values —
// the shape a [WebhookSecret]'s masked representation shows for "what
// headers would this request send," without the Authorization value (or
// anything else) a header might carry.
func maskedHeaderNames(headers map[string]string) []string {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
