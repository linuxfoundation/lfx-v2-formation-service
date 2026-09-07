// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package main wires up the HTTP server.
package main

import (
	"context"
	"log/slog"
	"net/http"

	diservice "github.com/linuxfoundation/lfx-v2-formation-service/cmd/formation-api/service"
	svcsvr "github.com/linuxfoundation/lfx-v2-formation-service/gen/http/lfx_v2_formation_service/server"
	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/config"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/middleware"
	"github.com/linuxfoundation/lfx-v2-formation-service/pkg/constants"
	"github.com/linuxfoundation/lfx-v2-formation-service/pkg/log"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"goa.design/clue/debug"
	goahttp "goa.design/goa/v3/http"
)

// StartServer initializes and starts the HTTP server.
func StartServer(ctx context.Context, cfg *config.Config) error {
	svcImpl, closeFn, err := diservice.New(ctx, cfg)
	if err != nil {
		return err
	}

	endpoints := svc.NewEndpoints(svcImpl)
	if cfg.Debug {
		endpoints.Use(debug.LogPayloads())
	}

	return handleHTTPServer(ctx, cfg, endpoints, closeFn)
}

func handleHTTPServer(ctx context.Context, cfg *config.Config, endpoints *svc.Endpoints, closeFn func() error) error {
	mux := goahttp.NewMuxer()
	if cfg.Debug {
		debug.MountPprofHandlers(debug.Adapt(mux))
		debug.MountDebugLogEnabler(debug.Adapt(mux))
	}

	eh := errorHandler(ctx)
	server := svcsvr.New(
		endpoints,
		mux,
		goahttp.RequestDecoder,
		goahttp.ResponseEncoder,
		eh,
		nil,
	)
	svcsvr.Mount(mux, server)

	var handler http.Handler = mux
	handler = middleware.RequestIDMiddleware()(handler)
	if cfg.Debug {
		handler = debug.HTTP()(handler)
	}
	handler = otelhttp.NewHandler(handler, "lfx-v2-formation-service")

	srv := &http.Server{
		Addr:              cfg.ServerAddress(),
		Handler:           handler,
		ReadHeaderTimeout: constants.DefaultReadHeaderTimeout,
		WriteTimeout:      constants.DefaultWriteTimeout,
		IdleTimeout:       constants.DefaultIdleTimeout,
	}

	return runServerWithContext(ctx, srv, closeFn)
}

func errorHandler(logCtx context.Context) func(context.Context, http.ResponseWriter, error) {
	return func(ctx context.Context, _ http.ResponseWriter, err error) {
		slog.ErrorContext(ctx, "HTTP error occurred", log.ErrKey, err, "outer_context", logCtx)
	}
}

func runServerWithContext(ctx context.Context, srv *http.Server, closeFn func() error) error {
	serverErr := make(chan error, 1)

	go func() {
		slog.InfoContext(ctx, "LFX V2 Formation Service listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return err
	case <-ctx.Done():
		slog.InfoContext(ctx, "shutdown initiated")
	}

	// Drain in-flight requests before releasing the Postgres pool: closing
	// it concurrently with Shutdown risks a mid-flight request seeing a
	// closed pool instead of completing.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), constants.DefaultShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.ErrorContext(ctx, "HTTP server shutdown error", log.ErrKey, err)
	}

	if err := closeFn(); err != nil {
		slog.ErrorContext(ctx, "service close error", log.ErrKey, err)
	}

	return nil
}
