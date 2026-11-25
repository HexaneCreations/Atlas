package notification

import "context"

// Sender delivers one delivery record to one channel. Implementations are
// per [ChannelType] and register under their own type without changing
// [Engine].
type Sender interface {
	Send(ctx context.Context, channel Channel, d Delivery) error
}
