package notification

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"

	"github.com/hexane/atlas/internal/platform/errs"
)

// Fixed texts [SendErrorCategory] reports. None is built from a URL, a
// header, a secret or anything a destination sent back.
const (
	categoryUnsafe      = "the destination is not a public address"
	categoryTimeout     = "the destination did not respond in time"
	categoryDNS         = "the destination host could not be resolved"
	categoryTLS         = "the TLS connection to the destination failed"
	categoryConnect     = "could not connect to the destination"
	categoryRequest     = "the request to the destination failed"
	categoryTemplate    = "the template could not be rendered; it may only use msg, eventJSON, nodeJSON, monitorJSON and ruleJSON"
	categoryConfig      = "the channel's destination, headers or template are not valid"
	categorySecret      = "the stored channel secret cannot be decrypted with the configured encryption key"
	categoryNoSecret    = "the channel has no secret configured"
	categoryUnspecified = "the notification could not be sent"
)

// ErrNoSender means no [Sender] is registered for a channel's type.
var ErrNoSender = errors.New("no sender registered for channel type")

func noSenderError(t ChannelType) error { return fmt.Errorf("%w %q", ErrNoSender, t) }

// ErrTestClaimed means the background dispatcher picked up a test delivery
// before [Engine.SendTest] could send it itself. It is still sent.
var ErrTestClaimed = errors.New("the test was picked up by the background dispatcher; its result is in the delivery log")

// SendError is a failed delivery. Error is the full, redacted text kept for
// the delivery log; category is the fixed text safe to show on its own.
type SendError struct{ msg, category string }

func (e *SendError) Error() string { return e.msg }

// statusError reports a destination's non-2xx response. The redacted body
// snippet is in the text, never in the category.
func statusError(what string, status int, body []byte) error {
	return &SendError{
		msg:      fmt.Sprintf("%s returned status %d: %s", what, status, Redact(string(body))),
		category: fmt.Sprintf("the destination returned status %d", status),
	}
}

func transportCategory(err error) string {
	var (
		netErr    net.Error
		dnsErr    *net.DNSError
		opErr     *net.OpError
		verifyErr *tls.CertificateVerificationError
		recordErr tls.RecordHeaderError
		alertErr  tls.AlertError
		hostErr   x509.HostnameError
		authErr   x509.UnknownAuthorityError
	)
	switch {
	case errors.Is(err, ErrUnsafeAddress):
		return categoryUnsafe
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return categoryTimeout
	case errors.As(err, &dnsErr):
		return categoryDNS
	case errors.As(err, &verifyErr), errors.As(err, &recordErr), errors.As(err, &alertErr),
		errors.As(err, &hostErr), errors.As(err, &authErr):
		return categoryTLS
	case errors.As(err, &opErr):
		return categoryConnect
	default:
		return categoryRequest
	}
}

// SendErrorCategory reduces a failed send to a short fixed message for an
// API response or a log line. [Redact] is not enough there: a destination's
// response, or the first line of a malformed one, can echo the request's
// path or headers in a shape no pattern recognises.
func SendErrorCategory(err error) string {
	var sendErr *SendError
	switch {
	case errors.As(err, &sendErr):
		return sendErr.category
	case errors.Is(err, ErrNoSender):
		return err.Error()
	case errors.Is(err, ErrRedirectRefused):
		return ErrRedirectRefused.Error()
	case errors.Is(err, ErrRequestTooLarge):
		return ErrRequestTooLarge.Error()
	case errors.Is(err, ErrTestClaimed):
		return ErrTestClaimed.Error()
	case errors.Is(err, ErrSecretUnreadable):
		return categorySecret
	}
	switch errs.CodeOf(err) {
	case errs.CodeInvalidArgument:
		return categoryConfig
	case errs.CodeFailedPrecondition:
		return categoryNoSecret
	default:
		return categoryUnspecified
	}
}
