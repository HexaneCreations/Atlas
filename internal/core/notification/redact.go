package notification

import (
	"regexp"
	"strings"

	"github.com/hexane/atlas/internal/platform/redact"
)

// Placeholder replaces a redacted value.
const Placeholder = redact.Placeholder

// sensitiveQueryParams are the URL query parameter names [Redact] replaces,
// matched case-insensitively. A Teams Workflows (Power Automate) URL
// carries its credential as sig=; sv, se and sp (API version, expiry,
// permissions) are not secrets and are left visible, since they are useful
// for diagnosing a delivery failure.
var sensitiveQueryParams = map[string]bool{
	"sig": true, "token": true, "key": true, "secret": true,
	"code": true, "api_key": true, "signature": true,
}

// queryParamPattern matches one query-string key=value pair. It is applied
// to the raw string rather than to a parsed URL, so it also finds a URL
// embedded inside other text: a wrapped *url.Error's message quotes the
// full request URL, and a misbehaving destination occasionally echoes the
// request URL back in its response body.
//
// This is the second line of defense, not the first: a [Sender] is
// expected to never put the destination URL into an error in the first
// place (see wrapSendError in httpclient.go, which unwraps a *url.Error
// down to its operation and host). This pattern, and urlPattern below, are
// what catch a URL that reaches Redact anyway — through a transport error
// this package did not originate, or a destination's response body
// echoing the request back.
var queryParamPattern = regexp.MustCompile(`(?i)([?&])([A-Za-z0-9_-]+)=([^&\s"'<>]*)`)

// urlPattern matches an http(s) URL's scheme and host (group 1), stopping
// at the first "/" or "?", and everything after it up to the next
// whitespace or quote (group 2). Query parameters are not the only place a
// webhook credential lives: Slack and Discord both encode a bearer-style
// token in the URL *path*, and a Teams Workflow's id is also path-carried
// alongside its query sig=. Rather than enumerate every shape, Redact
// reduces the whole path-and-query to one placeholder and keeps only the
// scheme and host, which are useful for diagnosing a failure and carry no
// credential.
var urlPattern = regexp.MustCompile(`(https?://[^/\s"'<>]+)(/[^\s"'<>]*|\?[^\s"'<>]*)`)

// Redact removes values a channel destination's URL, a wrapped request
// error, or a response snippet could carry: any https(s) URL's path and
// query (reduced to scheme://host/[REDACTED] by urlPattern), any
// remaining bare query parameter named in sensitiveQueryParams (for text
// that carries a query string without a full URL in front of it), and
// anything [redact.String] recognises as an Authorization header, bearer
// token, or URL userinfo credential.
//
// Every error a [Sender] returns, and every log line that might otherwise
// quote one, passes through this first — a webhook's signed or
// path-tokened URL, or a custom header's bearer token, must never reach a
// log file or (by way of the delivery's last_error, sanitised again on the
// way into storage) reach an operator who can only read, not configure,
// notifications. Safe to call on text that is not a URL at all, and on
// text that has already been redacted.
func Redact(s string) string {
	if s == "" {
		return s
	}
	s = urlPattern.ReplaceAllString(s, "${1}/"+Placeholder)
	s = queryParamPattern.ReplaceAllStringFunc(s, func(m string) string {
		sub := queryParamPattern.FindStringSubmatch(m)
		if sensitiveQueryParams[strings.ToLower(sub[2])] {
			return sub[1] + sub[2] + "=" + Placeholder
		}
		return m
	})
	return redact.String(s)
}
