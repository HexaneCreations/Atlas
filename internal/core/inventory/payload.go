package inventory

import (
	"encoding/json"
	"time"

	"github.com/hexane/atlas/internal/core/transport"
	"github.com/hexane/atlas/internal/platform/errs"
)

// Payload is one subject's snapshot in transit — the [transport.Payload] an
// agent sends for [transport.KindInventory].
type Payload struct {
	Subject     Subject         `json:"subject"`
	ObservedAt  time.Time       `json:"observed_at"`
	ContentHash string          `json:"content_hash"`
	Data        json.RawMessage `json:"data"`
}

// Kind implements [transport.Payload].
func (Payload) Kind() transport.Kind { return transport.KindInventory }

// Validate implements [transport.Payload].
func (p Payload) Validate() error {
	if p.Subject == "" {
		return errs.New(errs.CodeInvalidArgument, "inventory payload has no subject")
	}
	if p.ObservedAt.IsZero() {
		return errs.New(errs.CodeInvalidArgument, "inventory payload has no observed_at")
	}
	if p.ContentHash == "" {
		return errs.New(errs.CodeInvalidArgument, "inventory payload has no content hash")
	}
	return nil
}

func init() {
	transport.RegisterKind(transport.KindInventory, transport.ClassSnapshot, func(raw json.RawMessage) (transport.Payload, error) {
		var p Payload
		if len(raw) == 0 {
			return p, nil
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		return p, nil
	})
}
