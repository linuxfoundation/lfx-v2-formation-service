// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package nats hosts the NATS-backed implementations of this service's outbound
// ports. The transport shape mirrors lfx-v2-newsletter-service's NATS client,
// which in turn follows committee-service's, so a change to the pattern
// propagates rather than diverging per service.
package nats

import (
	"context"
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
// ctx, so a slow upstream cannot outlast what the caller was willing to wait —
// which matters because these calls run inside an open transaction.
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
