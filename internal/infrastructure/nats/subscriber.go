// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// Subscriber adapts the client's queue subscription to the domain port.
//
// A wrapper rather than the client implementing the port directly, following
// IndexerPublisher: the client is the transport and knows about subjects,
// headers and tracing, while the port is the one capability a use case is
// allowed to reach for. Keeping them apart is what stops the service layer
// acquiring the ability to publish because it needed to subscribe.
type Subscriber struct {
	client *Client
}

var _ port.Subscriber = (*Subscriber)(nil)

// NewSubscriber wires a subscriber onto an existing client connection.
func NewSubscriber(client *Client) *Subscriber {
	return &Subscriber{client: client}
}

// Subscribe delivers messages on subject to handler, one member of queue
// handling each.
func (s *Subscriber) Subscribe(
	ctx context.Context, subject, queue string, handler func(ctx context.Context, data []byte),
) (func(), error) {
	return s.client.QueueSubscribe(ctx, subject, queue, handler)
}
