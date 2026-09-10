// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package nats hosts the NATS-backed implementations of this service's outbound
// ports. The transport shape mirrors lfx-v2-newsletter-service's NATS client,
// which in turn follows committee-service's, so a change to the pattern
// propagates rather than diverging per service.
package nats

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Config configures the NATS client connection.
type Config struct {
	URL           string
	Timeout       time.Duration
	MaxReconnect  int
	ReconnectWait time.Duration
}

// Client wraps the NATS connection and provides request/reply infrastructure.
type Client struct {
	conn    *nats.Conn
	timeout time.Duration
}

// New connects a client. Reconnect handlers log at WARN and INFO so a gap in
// this service's NATS calls can be correlated with upstream availability rather
// than looking like a fault here.
func New(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.ReconnectWait <= 0 {
		cfg.ReconnectWait = 2 * time.Second
	}
	// MaxReconnect: negative means "reconnect forever" in nats.go semantics,
	// which is what an unset value should mean here — a service that stops
	// retrying after a fixed count never recovers from a long NATS outage.
	if cfg.MaxReconnect == 0 {
		cfg.MaxReconnect = -1
	}

	conn, err := nats.Connect(cfg.URL,
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(cfg.MaxReconnect),
		nats.ReconnectWait(cfg.ReconnectWait),
		nats.DisconnectErrHandler(func(_ *nats.Conn, dErr error) {
			slog.WarnContext(ctx, "NATS disconnected", "error", dErr)
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			slog.InfoContext(ctx, "NATS reconnected", "url", nc.ConnectedUrl())
		}),
		// Without this, shedding is silent. A subscription that hits its
		// pending limit drops messages and carries on, and from inside the
		// service that is indistinguishable from a quiet subject — the only
		// evidence would be checklists that appear a day late, which reads as
		// a listener problem rather than a load one. The slow-consumer error
		// is the moment it starts, so it is worth a line of its own.
		nats.ErrorHandler(func(_ *nats.Conn, sub *nats.Subscription, aErr error) {
			subject := ""
			if sub != nil {
				subject = sub.Subject
			}
			if errors.Is(aErr, nats.ErrSlowConsumer) {
				slog.WarnContext(ctx, "NATS is shedding messages for a slow consumer; "+
					"whatever is dropped is repaired by the next reconcile sweep",
					"subject", subject)
				return
			}
			slog.ErrorContext(ctx, "NATS asynchronous error", "subject", subject, "error", aErr)
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("connecting to NATS at %s: %w", cfg.URL, err)
	}

	slog.InfoContext(ctx, "NATS connected", "url", conn.ConnectedUrl())
	return &Client{conn: conn, timeout: cfg.Timeout}, nil
}

// Close drains and closes the connection.
func (c *Client) Close() {
	if c.conn != nil {
		_ = c.conn.Drain()
	}
}

// IsReady reports whether the connection is usable, for the readiness probe.
func (c *Client) IsReady() error {
	if c.conn == nil || !c.conn.IsConnected() || c.conn.IsDraining() {
		return fmt.Errorf("NATS client is not ready")
	}
	return nil
}

// Request sends a synchronous request and returns the raw reply bytes.
//
// The deadline is the lesser of the client timeout and any deadline already on
// ctx, so a slow upstream cannot outlast what the caller was willing to wait.
// Callers keep these calls outside their transactions, so the bound is on the
// request's own latency rather than on how long a row stays locked.
func (c *Client) Request(ctx context.Context, subject string, data []byte) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	reqCtx, span := tracer.Start(reqCtx, "nats.request",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("messaging.system", "nats"),
			attribute.String("messaging.destination.name", subject),
			attribute.Int("messaging.message.body.size", len(data)),
		),
	)
	defer span.End()

	msg := nats.NewMsg(subject)
	msg.Header = make(nats.Header)
	msg.Data = data
	// No caller token travels on the wire. Trust is enforced upstream of NATS,
	// so these reads are made as the service and never on behalf of whoever
	// happens to be looking — which is what lets the reconcile read a project
	// no requester is attached to.
	otel.GetTextMapPropagator().Inject(reqCtx, natsHeaderCarrier(msg.Header))

	reply, err := c.conn.RequestMsgWithContext(reqCtx, msg)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("NATS request to %s failed: %w", subject, err)
	}
	return reply.Data, nil
}

// Publish sends a message and does not wait for a reply.
//
// A nil error means the message reached this connection's write buffer, not that
// anything consumed it — core NATS has no acknowledgement and there is nobody to
// send one. Every caller here is publishing a projection that the reconcile
// republishes each sweep, so at-most-once delivery is the right trade: a lost
// message costs one tick of staleness, where JetStream would cost a stream to
// provision and monitor for data that is derived and rebuildable.
//
// Flushed before returning, so a publish immediately before shutdown is not
// silently dropped when the connection closes. Without it the buffer is
// discarded and the error surfaces nowhere.
func (c *Client) Publish(ctx context.Context, subject string, data []byte) error {
	pubCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	pubCtx, span := tracer.Start(pubCtx, "nats.publish",
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.String("messaging.system", "nats"),
			attribute.String("messaging.destination.name", subject),
			attribute.Int("messaging.message.body.size", len(data)),
		),
	)
	defer span.End()

	msg := nats.NewMsg(subject)
	msg.Header = make(nats.Header)
	msg.Data = data
	otel.GetTextMapPropagator().Inject(pubCtx, natsHeaderCarrier(msg.Header))

	if err := c.conn.PublishMsg(msg); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("NATS publish to %s failed: %w", subject, err)
	}
	if err := c.conn.FlushWithContext(pubCtx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("NATS flush after publishing to %s failed: %w", subject, err)
	}
	return nil
}

// subPendingMsgs and subPendingBytes bound what one subscription will hold for a
// handler that is behind.
//
// Set explicitly because the library's defaults — 500,000 messages and 64MB —
// are effectively unbounded for a handler whose work is a database transaction.
// The case that matters is a full-catalogue reindex, which republishes every
// project at once: with the defaults the buffer grows to hold hundreds of
// thousands of messages this service will get to eventually, all of them
// describing states that are stale by the time it does. Shedding is the better
// outcome, and it is only the better outcome because the sweep picks up whatever
// was shed.
//
// Deliberately small enough that shedding actually happens under a burst. A
// limit that is never reached would be the defaults with extra ceremony.
const (
	subPendingMsgs  = 2048
	subPendingBytes = 8 * 1024 * 1024
)

// QueueSubscribe delivers messages on subject to handler, sharing the work
// across the members of queue so exactly one replica handles each message.
//
// The returned stop function unsubscribes and waits for an in-flight handler to
// finish, which is why it is a function rather than the caller keeping the
// subscription: the ordering — stop delivery, drain what is running, only then
// let the context go — is the part worth not asking every caller to remember.
//
// Handler panics are contained here. A message from another service is data this
// service did not construct, and the failure mode of letting a decode panic
// escape is that one malformed publish takes down a replica that is otherwise
// serving reads perfectly well.
func (c *Client) QueueSubscribe(
	ctx context.Context, subject, queue string, handler func(ctx context.Context, data []byte),
) (func(), error) {
	sub, err := c.conn.QueueSubscribe(subject, queue, func(msg *nats.Msg) {
		// Continue the publisher's trace rather than starting a fresh one. This
		// service already injects trace context on everything it sends, and
		// every other v2 service that consumes extracts it here; without the
		// extract, work caused by an event would be an orphan trace and the one
		// question worth asking of it — what change led to this checklist —
		// could not be answered.
		msgCtx := otel.GetTextMapPropagator().Extract(ctx, natsHeaderCarrier(msg.Header))
		msgCtx, span := tracer.Start(msgCtx, "nats.process",
			trace.WithSpanKind(trace.SpanKindConsumer),
			trace.WithAttributes(
				attribute.String("messaging.system", "nats"),
				attribute.String("messaging.destination.name", subject),
				attribute.String("messaging.operation.type", "process"),
				attribute.Int("messaging.message.body.size", len(msg.Data)),
			),
		)
		defer span.End()

		defer func() {
			if r := recover(); r != nil {
				slog.ErrorContext(msgCtx, "recovered from a panic while handling a message",
					"subject", msg.Subject, "queue", queue, "panic", r)
				span.RecordError(fmt.Errorf("panic while handling a message: %v", r))
				span.SetStatus(codes.Error, "panic while handling a message")
			}
		}()
		handler(msgCtx, msg.Data)
	})
	if err != nil {
		return nil, fmt.Errorf("NATS subscribe to %s failed: %w", subject, err)
	}

	if limitErr := sub.SetPendingLimits(subPendingMsgs, subPendingBytes); limitErr != nil {
		// Not fatal. The subscription works with the library defaults; it just
		// buffers far more than intended under a burst, which costs memory
		// rather than correctness.
		slog.WarnContext(ctx, "could not bound the subscription buffer; the library defaults apply",
			"subject", subject, "error", limitErr)
	}

	// QueueSubscribe only buffers the SUB, so without this the server may not
	// have registered the interest by the time this returns and a message
	// published in that window reaches nobody. Unlike a lost publish that costs
	// one tick of staleness, this loses every event until the buffer happens to
	// flush, which is a silent start rather than a failed one.
	flushCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if flushErr := c.conn.FlushWithContext(flushCtx); flushErr != nil {
		// Unwound rather than left attached: a subscription this service cannot
		// confirm is one it cannot report on, and the caller treats the error as
		// "not listening" either way.
		if unsubErr := sub.Unsubscribe(); unsubErr != nil {
			slog.WarnContext(ctx, "could not unsubscribe after a failed flush",
				"subject", subject, "error", unsubErr)
		}
		return nil, fmt.Errorf("NATS flush after subscribing to %s failed: %w", subject, flushErr)
	}

	slog.InfoContext(ctx, "NATS subscribed", "subject", subject, "queue", queue)

	return func() {
		// Read before draining: Dropped answers only for a live subscription,
		// and a drained one reports an error instead of the count.
		if dropped, dropErr := sub.Dropped(); dropErr == nil && dropped > 0 {
			slog.WarnContext(ctx, "messages were shed by this subscription",
				"subject", subject, "dropped", dropped)
		}

		// Drain rather than Unsubscribe: it stops new delivery and waits for
		// the messages already handed to the handler, where Unsubscribe would
		// discard them. One of those may be mid-transaction.
		if drainErr := sub.Drain(); drainErr != nil {
			slog.WarnContext(ctx, "could not drain the subscription",
				"subject", subject, "error", drainErr)
			return
		}
		for sub.IsValid() {
			time.Sleep(5 * time.Millisecond)
		}
	}, nil
}
