package monitor

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/hexane/atlas/internal/platform/errs"
)

// Limits on a monitor's secret.
const (
	MaxHeaders         = 20
	MaxHeaderNameLen   = 100
	MaxHeaderValueLen  = 4096
	MaxCredentialLen   = 4096
	MaxRequestBodySize = 64 * 1024
)

const redacted = "[REDACTED]"

// Secret is everything about a check that may carry a credential: custom
// header values, basic or bearer credentials and the request body. It is
// sealed at rest and never returned by the API, logged, audited or put in
// a notification; every formatting path prints a placeholder.
type Secret struct {
	Headers       map[string]string
	BasicUsername string
	BasicPassword string
	BearerToken   string
	Body          string
}

func (Secret) String() string               { return redacted }
func (Secret) GoString() string             { return redacted }
func (Secret) LogValue() slog.Value         { return slog.StringValue(redacted) }
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// Empty reports whether s holds nothing worth sealing.
func (s Secret) Empty() bool {
	return len(s.Headers) == 0 && s.BasicUsername == "" && s.BasicPassword == "" && s.BearerToken == "" && s.Body == ""
}

// secretWire is Secret without its redacting methods, for sealing only.
type secretWire struct {
	Headers       map[string]string `json:"headers,omitempty"`
	BasicUsername string            `json:"basic_username,omitempty"`
	BasicPassword string            `json:"basic_password,omitempty"`
	BearerToken   string            `json:"bearer_token,omitempty"`
	Body          string            `json:"body,omitempty"`
}

// EncodeSecret is the plaintext the repository seals.
func EncodeSecret(s Secret) ([]byte, error) {
	return json.Marshal(secretWire(s)) //nolint:gosec // the plaintext being sealed; never served or logged
}

// DecodeSecret reverses [EncodeSecret]. The error carries no secret.
func DecodeSecret(b []byte) (Secret, error) {
	var w secretWire
	if err := json.Unmarshal(b, &w); err != nil {
		return Secret{}, errs.New(errs.CodeInternal, "the monitor secret is not readable").WithOp("monitor.DecodeSecret")
	}
	return Secret(w), nil
}

// HeaderInput is one custom header as a client sends it. A nil Value keeps
// the value already stored for that name.
type HeaderInput struct {
	Name  string
	Value *string
}

// SecretInput is the secret part of a create or update. Every nil field
// keeps what is stored (nothing, on create); Headers, when not nil,
// replaces the whole header set.
type SecretInput struct {
	Headers       []HeaderInput
	HeadersSet    bool
	BasicUsername *string
	BasicPassword *string
	BearerToken   *string
	Body          *string
}

// hop-by-hop and framing headers a client must not set: Go computes them,
// or they would change where or how the request is sent.
var forbiddenHeaders = map[string]bool{
	"Host": true, "Content-Length": true, "Transfer-Encoding": true, "Connection": true, "Upgrade": true,
	"Te": true, "Trailer": true, "Keep-Alive": true, "Expect": true, "Proxy-Authorization": true,
	"Proxy-Connection": true, "Cookie2": true,
}

// MergeSecret applies in over stored for a monitor using authType and
// method, and returns the secret to seal with the header names to store.
// Credentials of an auth type no longer in use are dropped.
func MergeSecret(stored Secret, in SecretInput, authType AuthType, method Method) (Secret, []string, error) {
	out := Secret{Headers: map[string]string{}}
	names := []string{}
	if in.HeadersSet {
		if len(in.Headers) > MaxHeaders {
			return Secret{}, nil, invalid("headers", "at most %d custom headers", MaxHeaders)
		}
		storedByName := map[string]string{}
		for k, v := range stored.Headers {
			storedByName[http.CanonicalHeaderKey(k)] = v
		}
		for _, h := range in.Headers {
			name := http.CanonicalHeaderKey(strings.TrimSpace(h.Name))
			if err := validateHeaderName(name, authType); err != nil {
				return Secret{}, nil, err
			}
			if _, dup := out.Headers[name]; dup {
				return Secret{}, nil, invalid("headers", "header %q is listed twice", name)
			}
			var value string
			if h.Value == nil {
				v, ok := storedByName[name]
				if !ok {
					return Secret{}, nil, invalid("headers", "header %q has no stored value to keep; send its value", name)
				}
				value = v
			} else {
				value = *h.Value
			}
			if len(value) > MaxHeaderValueLen || strings.ContainsAny(value, "\r\n\x00") {
				return Secret{}, nil, invalid("headers", "the value of header %q is too long or contains a line break", name)
			}
			out.Headers[name] = value
			names = append(names, name)
		}
	} else {
		for k, v := range stored.Headers {
			name := http.CanonicalHeaderKey(k)
			if err := validateHeaderName(name, authType); err != nil {
				return Secret{}, nil, err
			}
			out.Headers[name] = v
			names = append(names, name)
		}
	}

	pick := func(in *string, stored string) string {
		if in != nil {
			return *in
		}
		return stored
	}
	switch authType {
	case AuthBasic:
		out.BasicUsername = pick(in.BasicUsername, stored.BasicUsername)
		out.BasicPassword = pick(in.BasicPassword, stored.BasicPassword)
		if out.BasicUsername == "" || strings.Contains(out.BasicUsername, ":") || len(out.BasicUsername) > MaxCredentialLen {
			return Secret{}, nil, invalid("basic_username", "basic authentication needs a user name without a colon")
		}
		if out.BasicPassword == "" || len(out.BasicPassword) > MaxCredentialLen {
			return Secret{}, nil, invalid("basic_password", "basic authentication needs a password")
		}
	case AuthBearer:
		out.BearerToken = pick(in.BearerToken, stored.BearerToken)
		if out.BearerToken == "" || len(out.BearerToken) > MaxCredentialLen || strings.ContainsAny(out.BearerToken, "\r\n\x00 ") {
			return Secret{}, nil, invalid("bearer_token", "bearer authentication needs a token without spaces or line breaks")
		}
	}

	out.Body = pick(in.Body, stored.Body)
	if len(out.Body) > MaxRequestBodySize {
		return Secret{}, nil, invalid("body", "the request body must be at most %d bytes", MaxRequestBodySize)
	}
	if out.Body != "" && method != MethodPost {
		return Secret{}, nil, invalid("body", "only a POST request sends a body")
	}
	if len(out.Headers) == 0 {
		out.Headers = nil
	}
	return out, names, nil
}

func validateHeaderName(name string, authType AuthType) error {
	if name == "" || len(name) > MaxHeaderNameLen || !validToken(name) {
		return invalid("headers", "a header name must be 1 to %d token characters", MaxHeaderNameLen)
	}
	if forbiddenHeaders[name] || strings.HasPrefix(name, "Proxy-") {
		return invalid("headers", "header %q cannot be set on a check", name)
	}
	if name == "Authorization" && authType != AuthNone {
		return invalid("headers", "remove the Authorization header or set authentication to none")
	}
	return nil
}

// validToken reports whether s is an RFC 9110 token.
func validToken(s string) bool {
	for _, r := range s {
		if r > 0x7e || r <= 0x20 || strings.ContainsRune(`"(),/:;<=>?@[\]{}`, r) {
			return false
		}
	}
	return textproto.CanonicalMIMEHeaderKey(s) != ""
}
