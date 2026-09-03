// Package bus is the message bus: persistent store plus live fan-out.
package bus

import (
	"context"
	"sync"
	"sync/atomic"

	"airc/pkg/proto"
	"airc/pkg/store"
)

// Bus posts messages to the store and to every live subscriber.
type Bus struct {
	Store *store.Store

	mu   sync.Mutex
	subs map[chan store.Message]struct{}
	// dropped counts broadcasts a full subscriber missed; exported as a metric.
	dropped atomic.Uint64
}

// New wraps a store.
func New(s *store.Store) *Bus {
	return &Bus{Store: s, subs: map[chan store.Message]struct{}{}}
}

// Post persists m then broadcasts it. m.ID and m.TS are filled in; an empty To is derived from
// a leading "@nick" and Tags from the body, so every writer follows the same convention.
func (b *Bus) Post(ctx context.Context, m *store.Message) error {
	if m.To == "" {
		m.To = proto.Address(m.Body)
	}
	if m.Tags == nil {
		m.Tags = proto.Tags(m.Body)
	}
	if err := b.Store.Insert(ctx, m); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- *m:
		default:
			// Slow subscriber: drop. SSE handlers re-read the store on every signal; the driver
			// would miss a wake, hence the counter.
			b.dropped.Add(1)
		}
	}
	return nil
}

// Dropped returns how many broadcasts were lost to full subscriber buffers.
func (b *Bus) Dropped() uint64 { return b.dropped.Load() }

// Subscribe returns a channel of live messages and a cancel func. buffer sizes the channel.
func (b *Bus) Subscribe(buffer int) (<-chan store.Message, func()) {
	ch := make(chan store.Message, buffer)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}
