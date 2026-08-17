// Package redact removes credential-looking values from strings Atlas
// collects verbatim — process command lines, cron commands, and anything
// else where an operator's argument may carry a password or token.
//
// The design goal is maximum operational visibility with no accidental
// credential disclosure. Command lines are how an operator tells one JVM
// from another, so blanket exclusion would remove real diagnostic value;
// what leaves the host instead is the argument's shape with its value
// replaced. `--password=hunter2` becomes `--password=[REDACTED]`, while
// `--port 8080`, `-Xmx4g`, and `-jar app.jar` pass through untouched.
//
// Redaction is a defence against accident, not against a determined
// operator: a secret passed in an unrecognised, application-specific flag
// still gets through, so it does not license treating collected command
// lines as non-sensitive.
package redact

import (
	"regexp"
	"strings"
)

// Placeholder replaces a redacted value. It is deliberately visible rather
// than silent — an operator reading an inventory needs to know a value was
// withheld, not be left wondering whether the flag was empty.
const Placeholder = "[REDACTED]"

// secretKeys are the argument/parameter names whose values are withheld.
// Matching is case-insensitive and covers both `--name=value` and
// `--name value` forms, with any leading dash count or none at all.
var secretKeys = []string{
	"password", "passwd", "pwd",
	"token", "access[-_]?token", "refresh[-_]?token", "auth[-_]?token", "id[-_]?token",
	"api[-_]?key", "apikey", "api[-_]?secret",
	"secret", "client[-_]?secret", "secret[-_]?key",
	"credential", "credentials",
	"private[-_]?key", "auth",
	"passphrase", "session[-_]?key", "signing[-_]?key",
	"aws[-_]?secret[-_]?access[-_]?key", "aws[-_]?access[-_]?key[-_]?id",
}

var (
	// keyValuePattern covers `--password=secret`, `password=secret`,
	// `-password:secret`, and the same with any dash prefix.
	keyValuePattern = regexp.MustCompile(`(?i)(^|[\s"'` + "`" + `])(-{0,2}(?:` + strings.Join(secretKeys, "|") + `))\s*[=:]\s*("[^"]*"|'[^']*'|\S+)`)

	// spaceSeparatedPattern covers `--password secret` and `-p secret`.
	// It requires a leading dash so a bare English word ("password" in a
	// log message) is not treated as a flag introducing a value.
	spaceSeparatedPattern = regexp.MustCompile(`(?i)(^|\s)(-{1,2}(?:` + strings.Join(secretKeys, "|") + `))\s+("[^"]*"|'[^']*'|[^\s-]\S*)`)

	// mysqlPasswordPattern covers the MySQL/MariaDB family's attached
	// short form, `-pSECRET`, which is the single most common way a
	// credential reaches a process table. `-p` alone (prompt for password)
	// carries no secret and is left alone.
	mysqlPasswordPattern = regexp.MustCompile(`(^|\s)(-p)([^\s-]\S*)`)

	// authHeaderPattern covers `Authorization: Bearer xyz` and its Basic,
	// token, and schemeless variants, which appear in curl invocations in
	// cron jobs. It runs before keyValuePattern, which would otherwise
	// match only up to the scheme and leave the credential itself intact.
	authHeaderPattern = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)((?:bearer|basic|token)\s+)?[^"'\s]+`)

	// bearerPattern covers a scheme and credential appearing without the
	// Authorization header name in front of them.
	bearerPattern = regexp.MustCompile(`(?i)(bearer|basic|token)\s+([A-Za-z0-9\-._~+/=]{8,})`)

	// urlCredentialPattern covers credentials embedded in a URL's userinfo,
	// e.g. postgres://user:secret@host/db.
	urlCredentialPattern = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^\s:/@]+):([^\s@/]+)@`)
)

// String returns s with every recognised credential value replaced by
// [Placeholder]. It is safe to call on an already-redacted string.
func String(s string) string {
	if s == "" {
		return s
	}

	s = authHeaderPattern.ReplaceAllString(s, "${1}${2}"+Placeholder)
	s = bearerPattern.ReplaceAllString(s, "${1} "+Placeholder)
	s = keyValuePattern.ReplaceAllString(s, "${1}${2}="+Placeholder)
	s = spaceSeparatedPattern.ReplaceAllString(s, "${1}${2} "+Placeholder)
	s = mysqlPasswordPattern.ReplaceAllString(s, "${1}${2}"+Placeholder)
	s = urlCredentialPattern.ReplaceAllString(s, "${1}:"+Placeholder+"@")
	return s
}

// Redactor applies [String] when enabled, and is the pass-through identity
// when not. Collectors hold one rather than branching on a boolean at every
// call site, so the disabled path cannot be reached by forgetting a check.
type Redactor struct{ enabled bool }

// New returns a Redactor. Redaction is on unless explicitly disabled: the
// default must be the safe one, since the cost of a missed secret is
// unrecoverable and the cost of an over-redacted argument is a support
// question.
func New(enabled bool) Redactor { return Redactor{enabled: enabled} }

// Enabled reports whether this Redactor redacts.
func (r Redactor) Enabled() bool { return r.enabled }

// String redacts s when this Redactor is enabled.
func (r Redactor) String(s string) string {
	if !r.enabled {
		return s
	}
	return String(s)
}
