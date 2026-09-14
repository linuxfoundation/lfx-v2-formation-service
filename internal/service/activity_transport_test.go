// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	goahttp "goa.design/goa/v3/http"

	svcsvr "github.com/linuxfoundation/lfx-v2-formation-service/gen/http/lfx_v2_formation_service/server"
	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
)

// These exercise the activity route through the generated transport rather
// than through the service method, which is the only place the question they
// answer can be answered: how a malformed item_uid is reported is decided by
// the generated decoder and the transport's error encoder, not by anything in
// internal/service. The design declares no BadRequest error on this route, so
// reading the DSL establishes nothing — the status has to be observed.

// mountActivityRoute stands up the generated HTTP server over a service wired
// to in-memory doubles, and returns it alongside the seeded formation.
func mountActivityRoute(t *testing.T) (http.Handler, *model.Formation) {
	t.Helper()

	s, formation := newChecklistTestService(t)
	s.auth = fixedPrincipalAuth{}

	mux := goahttp.NewMuxer()
	server := svcsvr.New(
		svc.NewEndpoints(s),
		mux,
		goahttp.RequestDecoder,
		goahttp.ResponseEncoder,
		func(context.Context, http.ResponseWriter, error) {},
		nil,
	)
	svcsvr.Mount(mux, server)
	return mux, formation
}

func activityRequest(t *testing.T, handler http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// A malformed item_uid is a client error, never a
// server error, and never a quietly unfiltered feed. Observed rather than
// assumed: dsl.Format(FormatUUID) makes this a decode-time rejection, which
// keeps the value away from a UUID column where a failed cast would surface
// as a 500.
func TestGetFormationActivityRejectsAMalformedItemUID(t *testing.T) {
	handler, formation := mountActivityRoute(t)

	// The third is a ULID, which is the plausible mistake here: an entry
	// carries both, and its ulid is the one a consumer already pages on.
	for _, malformed := range []string{"not-a-uuid", "123", "01ARZ3NDEKTSV4RRFFQ69G5FAV"} {
		t.Run(malformed, func(t *testing.T) {
			rec := activityRequest(t, handler,
				"/formations/"+formation.ProjectUID+"/activity?v=1&item_uid="+malformed)

			assert.Equal(t, http.StatusBadRequest, rec.Code,
				"a malformed item_uid must be a client error; 500 and 200 are both contract violations")

			// Not an unfiltered feed under a different guise: the body must
			// not carry entries at all.
			var body map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.NotContains(t, body, "entries",
				"the response carries a feed, so a malformed filter fell back to the unfiltered read")
		})
	}
}

// The counterpart, so the test above is known to be rejecting the value
// rather than the request: the same route with a well-formed item_uid is
// served. The item does not exist, so this is the 404 the existence check
// produces — which is still proof that decode accepted the parameter.
func TestGetFormationActivityAcceptsAWellFormedItemUID(t *testing.T) {
	handler, formation := mountActivityRoute(t)

	rec := activityRequest(t, handler,
		"/formations/"+formation.ProjectUID+"/activity?v=1&item_uid=0199d1f6-1f1a-7c4e-9e2a-3c5b7d9f1a2b")

	assert.Equal(t, http.StatusNotFound, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, itemNotFoundMessage, body["message"])
}

// A request that sends no item_uid is served
// exactly as before, which is the whole basis for shipping this unadopted.
//
// The second case pins a behaviour that is easy to mistake for the malformed
// case above: `?item_uid=` with an empty value is decoded as *absent*, not as
// invalid, because the transport cannot tell a missing optional string from an
// empty one. So it returns the whole feed rather than a 400. Asserted rather
// than left implicit because it is the one place the route is more permissive
// than it looks, and a consumer templating the parameter from an unset
// variable lands on exactly this path.
func TestGetFormationActivityWithoutAnItemUIDStillServesTheFeed(t *testing.T) {
	for name, target := range map[string]string{
		"omitted":     "?v=1",
		"empty value": "?v=1&item_uid=",
	} {
		t.Run(name, func(t *testing.T) {
			handler, formation := mountActivityRoute(t)

			rec := activityRequest(t, handler, "/formations/"+formation.ProjectUID+"/activity"+target)

			require.Equal(t, http.StatusOK, rec.Code)
			var body map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Contains(t, body, "entries")
		})
	}
}

// fixedPrincipalAuth is a port.Authenticator double that accepts any token.
// Declared here rather than reusing mock.AuthService, which reads the
// principal from an environment variable and so would make these tests
// depend on process state.
type fixedPrincipalAuth struct{}

func (fixedPrincipalAuth) ParsePrincipal(context.Context, string, *slog.Logger) (string, string, error) {
	return "tester", "tester@example.org", nil
}
