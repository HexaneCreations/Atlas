package notification

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strings"

	"github.com/hexane/atlas/internal/platform/errs"
)

// ChannelSecrets is the part of a [ChannelStore] needed to complete an edit
// that keeps some of a channel's stored secret.
type ChannelSecrets interface {
	GetChannel(ctx context.Context, id string) (Channel, error)
	ChannelSecret(ctx context.Context, channelID string) (string, error)
}

// Destination is what a reader may see of where a channel sends: the host,
// and header names without their values.
type Destination struct {
	Host        string
	HeaderNames []string
}

// DescribeDestination reads c's masked destination. It is empty for a channel
// with no readable secret, and for a type with no destination URL.
func DescribeDestination(ctx context.Context, store ChannelSecrets, c Channel) Destination {
	if !c.SecretConfigured || !hasDestination(c.Type) {
		return Destination{}
	}
	secret, err := store.ChannelSecret(ctx, c.ID)
	if err != nil {
		return Destination{}
	}
	s, err := decodeHTTPSecret(secret)
	if err != nil {
		return Destination{}
	}
	u, err := url.Parse(s.URL)
	if err != nil {
		return Destination{}
	}
	return Destination{Host: u.Host, HeaderNames: maskedHeaderNames(s.Headers)}
}

// ValidateNew checks a channel about to be created, without touching the
// network.
//
// Its error texts, like [PrepareUpdate]'s and [Engine.PrepareDraftTest]'s,
// are fixed and name the offending field in the "field" detail: a rejected
// request is logged, and must not carry a URL or header into the log.
func (in ChannelInput) ValidateNew() error {
	if err := in.Validate(); err != nil {
		return err
	}
	_, err := validateDestination(in.Type, in.Config, in.secret())
	return err
}

// PrepareUpdate validates an update of a saved channel, without touching the
// network, and returns it with any kept parts of the stored secret filled in.
// With no new secret only the rest of the channel is validated.
func PrepareUpdate(ctx context.Context, store ChannelSecrets, channelID string, in ChannelInput) (ChannelInput, error) {
	const op = "notification.PrepareUpdate"
	if err := in.Validate(); err != nil {
		return ChannelInput{}, err
	}
	saved, err := store.GetChannel(ctx, channelID)
	if err != nil {
		return ChannelInput{}, err
	}
	if !hasDestination(in.Type) && in.Secret != nil {
		return in, nil
	}
	if in.Secret == nil || strings.TrimSpace(*in.Secret) == "" {
		in.Secret = nil
		if in.Type != saved.Type {
			return ChannelInput{}, invalid(op, "type", "changing a channel's type needs a new secret")
		}
		return in, validateConfig(in.Type, in.Config)
	}

	secret, _, err := completeSecret(ctx, store, in, channelID)
	if errors.Is(err, ErrSecretUnreadable) {
		return ChannelInput{}, invalid(op, "url",
			"the stored secret cannot be decrypted with the configured encryption key; enter the URL and header values again")
	}
	if err != nil {
		return ChannelInput{}, err
	}
	if _, err := validateDestination(in.Type, in.Config, secret); err != nil {
		return ChannelInput{}, err
	}
	in.Secret = &secret
	return in, nil
}

func (in ChannelInput) secret() string {
	if in.Secret == nil {
		return ""
	}
	return *in.Secret
}

func invalid(op, field, text string) *errs.Error {
	return errs.New(errs.CodeInvalidArgument, "%s", text).WithOp(op).WithDetail("field", field)
}

func hasDestination(t ChannelType) bool { return t == ChannelTeams || t == ChannelWebhook }

// decodeHTTPSecret reads a teams or webhook secret; a teams secret is one
// with no headers.
func decodeHTTPSecret(secret string) (WebhookSecret, error) {
	var s WebhookSecret
	err := json.Unmarshal([]byte(secret), &s)
	return s, err
}

// completeSecret fills in what an edit leaves out of a channel's secret: all
// of it when the submitted one is empty, else its url when that is empty and,
// only then, the value of any header whose value is empty. keptFrom is the
// saved channel's id when anything was kept.
//
// Kept header values never accompany a new url: that would let an edit read
// them back by pointing them at a host of its choosing.
func completeSecret(ctx context.Context, store ChannelSecrets, in ChannelInput, channelID string) (secret, keptFrom string, err error) {
	const op = "notification.completeSecret"
	submitted := in.secret()
	if channelID == "" || !hasDestination(in.Type) && strings.TrimSpace(submitted) != "" {
		return submitted, "", nil
	}

	var s WebhookSecret
	if strings.TrimSpace(submitted) != "" {
		if s, err = decodeHTTPSecret(submitted); err != nil {
			return submitted, "", nil // reported by validation
		}
		if s.URL != "" {
			for _, value := range s.Headers {
				if in.Type == ChannelWebhook && value == "" {
					return "", "", invalid(op, "headers", "a new URL needs every header value entered again")
				}
			}
			return submitted, "", nil
		}
	}

	saved, err := store.GetChannel(ctx, channelID)
	if err != nil {
		return "", "", err
	}
	// A stored secret is only ever used as the kind of channel it was saved
	// for.
	if saved.Type != in.Type {
		return "", "", invalid(op, "type", "the saved channel is of another type")
	}
	stored, err := store.ChannelSecret(ctx, saved.ID)
	if err != nil {
		if !errors.Is(err, ErrSecretUnreadable) && errs.CodeOf(err) == errs.CodeFailedPrecondition {
			return "", "", invalid(op, "url", "the channel has no stored secret; supply one")
		}
		return "", saved.ID, err
	}
	if strings.TrimSpace(submitted) == "" {
		return stored, saved.ID, nil
	}

	kept, err := decodeHTTPSecret(stored)
	if err != nil {
		return "", "", invalid(op, "url", "the channel has no stored secret; supply one")
	}
	s.URL = kept.URL
	for name, value := range s.Headers {
		if value != "" {
			continue
		}
		v, ok := kept.Headers[name]
		if !ok {
			return "", "", invalid(op, "headers", "a header with no stored value needs one")
		}
		s.Headers[name] = v
	}
	merged, err := json.Marshal(s)
	if err != nil {
		return "", "", errs.New(errs.CodeInternal, "could not encode the channel secret").WithOp(op)
	}
	return string(merged), saved.ID, nil
}

// validateConfig checks a channel's non-secret config. Unknown keys are
// refused: config is stored and served as it is, so a URL or token put there
// would not be protected.
func validateConfig(t ChannelType, config map[string]any) error {
	const op = "notification.validateConfig"
	if !hasDestination(t) {
		return nil
	}
	for key := range config {
		if t != ChannelWebhook || key != "template" && key != "content_type" {
			return invalid(op, "config", "the channel config has a setting this channel type does not have")
		}
	}
	if t != ChannelWebhook {
		return nil
	}
	c, err := DecodeWebhookConfig(config)
	if err != nil {
		return err
	}
	return c.Validate()
}

// validateDestination runs the sender's own validation for the channel's type
// and returns the destination host, "" for a type with none.
func validateDestination(t ChannelType, config map[string]any, secret string) (string, error) {
	const op = "notification.validateDestination"
	if !hasDestination(t) {
		return "", nil
	}
	if err := validateConfig(t, config); err != nil {
		return "", err
	}
	if strings.TrimSpace(secret) == "" {
		return "", invalid(op, "url", "a secret is required")
	}

	var rawURL string
	if t == ChannelTeams {
		s, err := DecodeTeamsSecret(secret)
		if err != nil {
			return "", err
		}
		if err := validateDestinationURL(s.URL, nil, op); err != nil {
			return "", err
		}
		if s.Validate() != nil {
			return "", invalid(op, "url",
				"the destination host is not an allowed Teams host ("+strings.Join(TeamsAllowedHostSuffixes, ", ")+")")
		}
		rawURL = s.URL
	} else {
		s, err := DecodeWebhookSecret(secret)
		if err != nil {
			return "", err
		}
		if err := validateDestinationURL(s.URL, nil, op); err != nil {
			return "", err
		}
		if s.Validate() != nil {
			return "", invalid(op, "headers",
				"a custom header is not allowed: check the number, name, length and characters of the headers")
		}
		rawURL = s.URL
	}
	return publicHost(rawURL)
}

// publicHost returns rawURL's host, refusing one that is visibly not public:
// a non-public IP literal, localhost, or a numeric form a resolver may read
// as an IPv4 address (127.1, 0x7f000001). It resolves nothing; a name that
// resolves to a non-public address is refused when it is dialed.
func publicHost(rawURL string) (string, error) {
	refused := invalid("notification.publicHost", "url", categoryUnsafe)
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", refused
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return "", refused
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if !isSafePublicAddr(net.IP(addr.WithZone("").AsSlice())) {
			return "", refused
		}
		return host, nil
	}
	last := host[strings.LastIndex(host, ".")+1:]
	if strings.HasPrefix(last, "0x") || strings.Trim(last, "0123456789") == "" {
		return "", refused
	}
	return host, nil
}
