// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package query

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
)

// recordedCall is one request the fake read layer saw.
type recordedCall struct {
	path  string
	query url.Values
	auth  string
}

// fakeLayer stands in for the read layer. Handlers are chosen by path, so a
// test can make the count answer one thing and the list another — which is
// the only way to reach the race between the two calls.
type fakeLayer struct {
	t     *testing.T
	calls []recordedCall

	countBody string
	countCode int
	listBody  string
	listCode  int

	// listBodies answers successive list calls in order, for the tests about
	// paging. It takes precedence over listBody while it has entries left.
	listBodies []string
}

func (f *fakeLayer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls = append(f.calls, recordedCall{
		path:  r.URL.Path,
		query: r.URL.Query(),
		auth:  r.Header.Get("Authorization"),
	})

	code, body := f.listCode, f.listBody
	if r.URL.Path == "/query/resources/count" {
		code, body = f.countCode, f.countBody
	} else if len(f.listBodies) > 0 {
		body, f.listBodies = f.listBodies[0], f.listBodies[1:]
	}
	if code == 0 {
		code = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}

// newFake starts a fake read layer and a client pointed at it. The client is
// given a transport that stamps an Authorization header, standing in for the
// identity the provider layer wires in — the adapter never mints one itself.
func newFake(t *testing.T, layer *fakeLayer) (*Client, *httptest.Server) {
	t.Helper()
	layer.t = t
	srv := httptest.NewServer(layer)
	t.Cleanup(srv.Close)

	httpClient := &http.Client{Transport: bearer{token: "test-token", base: http.DefaultTransport}}
	client, err := NewClient(Config{BaseURL: srv.URL}, httpClient)
	require.NoError(t, err)
	return client, srv
}

type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

// Which resource types can be answered for at all. This is the fact the
// service's registry is built from, and it is asserted here because this is
// where it is decided.
//
// repository is absent on purpose and is the entry worth guarding: no service
// in the platform owns repositories, so there is nothing indexed to find. A
// lookup registered for it would resolve against nothing or fail every sweep,
// and either would be worse than the row being honestly manual.
func TestTheAdapterAnswersForExactlyTheTypesThatAreIndexedUnderAProject(t *testing.T) {
	got := SupportedResourceTypes()
	sort.Strings(got)
	assert.Equal(t, []string{"committee", "mailing_list"}, got)

	_, _, err := (&Client{}).Count(context.Background(), "project-1", "repository")
	require.Error(t, err, "repository must not be answerable")
}

// The two-call shape, and the order it happens in. The count is the predicate;
// the list only runs once the count says there is something to name.
func TestTheCountIsAskedFirstAndTheListOnlyWhenSomethingWasCounted(t *testing.T) {
	layer := &fakeLayer{
		countBody: `{"count":3,"has_more":false}`,
		listBody:  `{"resources":[{"type":"committee","id":"committee-1"}]}`,
	}
	client, _ := newFake(t, layer)

	count, ref, err := client.Count(context.Background(), "project-1", "committee")
	require.NoError(t, err)
	assert.Equal(t, 3, count)
	require.NotNil(t, ref)
	assert.Equal(t, "committee", ref.Type)
	assert.Equal(t, "committee-1", ref.UID)

	require.Len(t, layer.calls, 2)
	assert.Equal(t, "/query/resources/count", layer.calls[0].path, "the count must be asked first")
	assert.Equal(t, "/query/resources", layer.calls[1].path)

	// Identical filters are what make the count a valid predicate for the
	// list: the layer filters both by the same principal, so a disagreement
	// between them can only be a race and never a different question.
	for _, call := range layer.calls {
		assert.Equal(t, "1", call.query.Get("v"))
		assert.Equal(t, "committee", call.query.Get("type"))
		assert.Equal(t, "project:project-1", call.query.Get("parent"))
		assert.Equal(t, "Bearer test-token", call.auth, "the service identity must be presented")
	}
	assert.Equal(t, "", layer.calls[0].query.Get("page_size"), "the count takes no page size")
	assert.Equal(t, "50", layer.calls[1].query.Get("page_size"),
		"one visible hit is wanted, but the layer filters the page by access after building it")
	assert.Equal(t, "", layer.calls[1].query.Get("page_token"), "the first page is asked for without a token")

	// No ordering is imposed. The layer's default sort is already a total
	// order, so naming one here could only disagree with it.
	assert.Equal(t, "", layer.calls[1].query.Get("sort"))
}

// Nothing counted means nothing to name, so the list is never issued. Worth a
// test of its own: this is the common case on every project that has not done
// the work yet, and issuing a second call for it would double the sweep's load
// on the read layer for no possible answer.
func TestNothingCountedSkipsTheListEntirely(t *testing.T) {
	layer := &fakeLayer{countBody: `{"count":0,"has_more":false}`}
	client, _ := newFake(t, layer)

	_, _, err := client.Count(context.Background(), "project-1", "committee")
	require.Error(t, err)
	assert.Len(t, layer.calls, 1, "the list was issued with nothing to list")
}

// The mailing-list row asks about the individual list, not the project's
// Groups.io presence. Those are different indexed types and resolving against
// the wrong one would call the work done while it visibly is not.
func TestTheMailingListRowResolvesAgainstTheListRatherThanTheService(t *testing.T) {
	layer := &fakeLayer{
		countBody: `{"count":1,"has_more":false}`,
		listBody:  `{"resources":[{"type":"groupsio_mailing_list","id":"list-1"}]}`,
	}
	client, _ := newFake(t, layer)

	_, ref, err := client.Count(context.Background(), "project-1", "mailing_list")
	require.NoError(t, err)
	require.NotNil(t, ref)
	assert.Equal(t, "list-1", ref.UID)

	for _, call := range layer.calls {
		assert.Equal(t, "groupsio_mailing_list", call.query.Get("type"))
		assert.NotEqual(t, "groupsio_service", call.query.Get("type"))
	}
}

// The read is scoped to existence and counts. Contents are never parsed: doing
// so would couple this service to two donor schemas and widen what the
// identity is used for beyond what it was granted for.
//
// Checked by answering with a payload whose data field would break any
// attempt to read it. A client that parsed contents could not return cleanly
// from this.
func TestResourceContentsAreNeverParsed(t *testing.T) {
	layer := &fakeLayer{
		countBody: `{"count":1,"has_more":false}`,
		listBody:  `{"resources":[{"type":"committee","id":"committee-1","data":"not-an-object"}]}`,
	}
	client, _ := newFake(t, layer)

	count, ref, err := client.Count(context.Background(), "project-1", "committee")
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	require.NotNil(t, ref)
	assert.Equal(t, "committee-1", ref.UID)
}

// has_more means the count is a floor rather than an exact figure. That is
// harmless for this consumer — the thresholds compared against are one or two,
// and a floor that clears the minimum clears it — so it must not be treated as
// an error or as a reason to go back for more.
func TestAFlooredCountIsUsedAsIs(t *testing.T) {
	layer := &fakeLayer{
		countBody: `{"count":10,"has_more":true}`,
		listBody:  `{"resources":[{"type":"committee","id":"committee-1"}]}`,
	}
	client, _ := newFake(t, layer)

	count, _, err := client.Count(context.Background(), "project-1", "committee")
	require.NoError(t, err)
	assert.Equal(t, 10, count)
}

// A redirect is refused rather than followed.
//
// The token is attached by the OAuth transport inside RoundTrip, so it is
// reattached on every hop and the usual stripping of Authorization on a
// cross-host redirect does not protect it. A read layer that redirected to
// plaintext, or anywhere the configuration never named, would otherwise be
// handed the service identity.
func TestARedirectIsRefusedRatherThanFollowed(t *testing.T) {
	var elsewhereSaw string
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhereSaw = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":1,"has_more":false}`))
	}))
	t.Cleanup(elsewhere.Close)

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	httpClient := &http.Client{Transport: bearer{token: "test-token", base: http.DefaultTransport}}
	client, err := NewClient(Config{BaseURL: redirector.URL}, httpClient)
	require.NoError(t, err)

	_, _, err = client.Count(context.Background(), "project-1", "committee")
	require.Error(t, err, "the client followed a redirect while carrying the service identity")
	assert.Empty(t, elsewhereSaw, "the service identity was sent to the redirect target")
}

// An empty page carrying a token is not the end of the list.
//
// The read layer pages the raw index and applies access control to the page
// afterwards, while the count endpoint counts only what the caller may see. So
// a project whose first page holds nothing visible still has a resource the
// count promised, on a later page. Stopping at the empty page reports the row
// missing on every sweep for as long as the hidden resource sorts first — a row
// that never advances, and no error anywhere to say why.
func TestAnEmptyPageWithATokenIsFollowedRatherThanBelieved(t *testing.T) {
	layer := &fakeLayer{
		countBody: `{"count":1,"has_more":false}`,
		listBodies: []string{
			`{"resources":[],"page_token":"next"}`,
			`{"resources":[{"type":"committee","id":"committee-7"}]}`,
		},
	}
	client, _ := newFake(t, layer)

	count, ref, err := client.Count(context.Background(), "project-1", "committee")
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	require.NotNil(t, ref, "the client stopped at a page the access filter had emptied")
	assert.Equal(t, "committee-7", ref.UID)

	require.Len(t, layer.calls, 3, "the count, then both pages")
	assert.Equal(t, "next", layer.calls[2].query.Get("page_token"),
		"the second page was asked for without the token the first one returned")
}

// An empty page with no token is the end of the list, and means absent.
//
// The distinction from the test above is the whole reason the token is read: an
// exhausted list is the documented race with the count call, and reporting it
// as absent leaves the row pending for the next sweep.
func TestAnEmptyFinalPageIsReportedAsAbsent(t *testing.T) {
	layer := &fakeLayer{
		countBody: `{"count":1,"has_more":false}`,
		listBody:  `{"resources":[]}`,
	}
	client, _ := newFake(t, layer)

	_, _, err := client.Count(context.Background(), "project-1", "committee")
	require.ErrorIs(t, err, domain.ErrNotFound)
	assert.Len(t, layer.calls, 2, "the client kept asking after the list said it was done")
}

// A read layer that never stops issuing tokens is given up on rather than
// followed. The lookup's deadline already bounds the wall clock, so the cap is
// about not turning one sweep into a request loop against the read layer.
func TestAnEndlessRunOfEmptyPagesIsGivenUpOn(t *testing.T) {
	layer := &fakeLayer{
		countBody: `{"count":1,"has_more":false}`,
		listBody:  `{"resources":[],"page_token":"always-more"}`,
	}
	client, _ := newFake(t, layer)

	_, _, err := client.Count(context.Background(), "project-1", "committee")
	require.ErrorIs(t, err, domain.ErrNotFound)
	assert.Len(t, layer.calls, 1+maxListPages, "the client did not stop paging")
}
