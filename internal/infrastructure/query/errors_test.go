// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package query

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
)

// The distinction this whole file exists to protect.
//
// The resolution pass branches on exactly one thing: whether the error is
// domain.ErrNotFound. Not-found means the work has not been done yet and the
// row is pending, which is the ordinary state of most rows on most projects.
// Anything else means the sweep could not find out, and the row is counted as
// a failure so somebody sees it.
//
// Getting this backwards is not a visible bug. An identity that stops being
// accepted returns 401 on every call, and mapped to not-found that reads as
// every project in the platform having done no work — silently, on every
// sweep, with no error anywhere. That is the failure this table prevents, and
// it is why the mapping is tested per status rather than in the aggregate.
func TestTheErrorTaxonomySeparatesNotYetDoneFromCouldNotFindOut(t *testing.T) {
	tests := []struct {
		name string

		countCode int
		countBody string
		listCode  int
		listBody  string

		// wantNotFound is the whole assertion: true means the pass counts the
		// row pending, false means it counts it failed.
		wantNotFound bool
		because      string
	}{
		{
			name:         "nothing exists yet",
			countBody:    `{"count":0,"has_more":false}`,
			wantNotFound: true,
			because:      "work still to do is the ordinary case, not a fault",
		},
		{
			name:         "counted, then gone before it could be named",
			countBody:    `{"count":1,"has_more":false}`,
			listBody:     `{"resources":[]}`,
			wantNotFound: true,
			because: "a race between the two calls resolves itself on the next sweep; " +
				"counting it a fault would make an ordinary deletion look like a broken sweep",
		},
		{
			name:         "the identity is not accepted",
			countCode:    http.StatusUnauthorized,
			countBody:    `{"message":"unauthorized"}`,
			wantNotFound: false,
			because: "an identity that stopped working must never read as every project " +
				"having done no work",
		},
		{
			name:         "the identity is accepted but not permitted",
			countCode:    http.StatusForbidden,
			countBody:    `{"message":"forbidden"}`,
			wantNotFound: false,
			because:      "a missing grant is a deployment problem, and silence would hide it",
		},
		{
			name:         "the call was malformed",
			countCode:    http.StatusBadRequest,
			countBody:    `{"message":"bad request"}`,
			wantNotFound: false,
			because:      "a bug in this adapter should be loud",
		},
		{
			name:         "the read layer is broken",
			countCode:    http.StatusInternalServerError,
			countBody:    `{"message":"boom"}`,
			wantNotFound: false,
			because:      "unreachable is not the same as empty",
		},
		{
			name:         "the read layer is broken only on the second call",
			countBody:    `{"count":1,"has_more":false}`,
			listCode:     http.StatusInternalServerError,
			listBody:     `{"message":"boom"}`,
			wantNotFound: false,
			because:      "a failure after a successful count is still a failure",
		},
		{
			name:         "the response is not what was promised",
			countBody:    `{"count":`,
			wantNotFound: false,
			because:      "an undecodable answer is no answer",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := newFake(t, &fakeLayer{
				countCode: tc.countCode,
				countBody: tc.countBody,
				listCode:  tc.listCode,
				listBody:  tc.listBody,
			})

			_, ref, err := client.Count(context.Background(), "project-1", "committee")
			require.Error(t, err)
			assert.Nil(t, ref, "no reference may be returned alongside an error")
			assert.Equal(t, tc.wantNotFound, errors.Is(err, domain.ErrNotFound), tc.because)
		})
	}
}

// A rejected identity is distinguishable from any other fault, so an operator
// reading a failing sweep can tell "the grant is wrong" from "the layer is
// down" without opening a trace.
func TestARejectedIdentityIsIdentifiableAsSuch(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		client, _ := newFake(t, &fakeLayer{countCode: code, countBody: `{}`})

		_, _, err := client.Count(context.Background(), "project-1", "committee")
		require.Error(t, err)
		assert.True(t, errors.Is(err, domain.ErrForbidden))
		assert.False(t, errors.Is(err, domain.ErrNotFound))
	}
}

// An unreachable layer is a fault rather than an empty answer. Same reasoning
// as a 401, reached a different way: nothing answered at all.
func TestAnUnreachableLayerIsAFaultRatherThanAnEmptyAnswer(t *testing.T) {
	client, srv := newFake(t, &fakeLayer{countBody: `{"count":0}`})
	srv.Close()

	_, _, err := client.Count(context.Background(), "project-1", "committee")
	require.Error(t, err)
	assert.False(t, errors.Is(err, domain.ErrNotFound))
}

// A client with no address is refused at construction rather than at the first
// sweep. Unconfigured is a supported state, but it is expressed by registering
// no lookups at all, never by a client that points nowhere.
func TestAClientWithNoAddressIsRefused(t *testing.T) {
	_, err := NewClient(Config{}, nil)
	require.Error(t, err)
}
