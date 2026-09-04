// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package design defines the Goa API and service endpoints.
// Run `make apigen` to regenerate the code in gen/ after modifying this file.
package design

import "goa.design/goa/v3/dsl"

var _ = dsl.API("lfx-v2-formation-service", func() {
	dsl.Title("LFX V2 Formation Service")
	dsl.Version("1.0")
})

var _ = dsl.Service("lfx_v2_formation_service", func() {
	dsl.Description("LFX V2 Formation Service health endpoints")

	dsl.Method("livez", func() {
		dsl.Description("Liveness probe.")
		dsl.Meta("swagger:generate", "false")
		dsl.Result(dsl.Bytes, func() { dsl.Example("OK") })
		dsl.HTTP(func() {
			dsl.GET("/livez")
			dsl.Response(dsl.StatusOK, func() { dsl.ContentType("text/plain") })
		})
	})

	dsl.Method("readyz", func() {
		dsl.Description("Readiness probe.")
		dsl.Meta("swagger:generate", "false")
		dsl.Result(dsl.Bytes, func() { dsl.Example("OK") })
		dsl.Error("ServiceUnavailable", ServiceUnavailableError, "Service unavailable")
		dsl.HTTP(func() {
			dsl.GET("/readyz")
			dsl.Response(dsl.StatusOK, func() { dsl.ContentType("text/plain") })
			dsl.Response("ServiceUnavailable", dsl.StatusServiceUnavailable)
		})
	})
})

// ServiceUnavailableError is the DSL type for a service unavailable error.
var ServiceUnavailableError = dsl.Type("ServiceUnavailableError", func() {
	dsl.Attribute("code", dsl.String, "HTTP status code", func() { dsl.Example("503") })
	dsl.Attribute("message", dsl.String, "Error message", func() { dsl.Example("The service is unavailable.") })
	dsl.Required("code", "message")
})
