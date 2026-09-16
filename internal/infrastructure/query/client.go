// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package query reads the shared read layer on this service's own behalf, to
// answer the checklist rows whose status the platform can determine for
// itself.
//
// This is the service's first outbound HTTP dependency. Everything else it
// reads, it reads over NATS; the read layer is the documented exception,
// because it is the only place that can answer "does this project have one of
// these" without every owning service growing a project-scoped lookup it does
// not have today.
//
// Nothing here parses resource contents. The question is existence and count,
// and the answer is a number and one identifier — widening that would couple
// this service to the donor schemas and quietly widen what the service
// identity is used for.
package query

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/pkg/constants"
)

// The read layer's API version. One value is accepted and it is not optional.
const apiVersion = "1"

// indexedTypes maps the platform-neutral name a template row carries to the
// name the read layer indexes it under. This is the only place in the service
// that knows what the read layer calls things; the template stays neutral.
//
// mailing_list resolves to the individual list, deliberately not to
// groupsio_service. A project's Groups.io presence can exist with no lists in
// it, so resolving the row against the service would call the work done while
// it visibly is not.
var indexedTypes = map[string]string{
	"committee":    "committee",
	"mailing_list": "groupsio_mailing_list",
}

// Config addresses the read layer.
type Config struct {
	// BaseURL is the service root, e.g. http://lfx-v2-query-service:8080.
	BaseURL string

	// Timeout bounds one lookup, both calls included.
	Timeout time.Duration
}

// Client answers project-scoped existence questions against the read layer.
type Client struct {
	baseURL *url.URL
	client  *http.Client

	// timeout bounds a whole lookup rather than a single request. Count issues
	// two, so the client's own per-request timeout would let one lookup hold
	// the caller's row lock for twice this.
	timeout time.Duration
}

// Compile-time check that this satisfies the port. Worth stating explicitly:
// the provider returns the interface, so a signature drifting out of line
// would otherwise surface as a nil checker at runtime, which reads exactly
// like the unconfigured state this service supports on purpose.
var _ port.ResourceChecker = (*Client)(nil)

// NewClient builds a client against the read layer. The supplied *http.Client
// MUST already carry the service identity's Authorization — it is wired in the
// provider layer from the M2M token source, never taken from a caller's
// request, because a sweep has no caller.
func NewClient(cfg Config, httpClient *http.Client) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("query service base URL is empty")
	}
	base, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid query service base URL: %w", err)
	}
	// url.Parse accepts far more than this needs: "query-service:8080" parses
	// as a scheme with an opaque body, and "/query" as a path. Either is
	// rejected here rather than at the first sweep, where it would surface as
	// the read layer being unreachable and send someone looking at the network.
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("query service base URL must be http or https, got %q in %q", base.Scheme, cfg.BaseURL)
	}
	if base.Host == "" {
		return nil, fmt.Errorf("query service base URL has no host: %q", cfg.BaseURL)
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = constants.DefaultQueryServiceTimeout
	}
	if httpClient == nil {
		httpClient = &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}
	}
	if httpClient.Timeout == 0 {
		httpClient.Timeout = cfg.Timeout
	}
	return &Client{baseURL: base, client: httpClient, timeout: cfg.Timeout}, nil
}

// SupportedResourceTypes lists the template resource types this client can
// answer for, so the wiring layer registers exactly these and the service
// reports anything else unanswerable rather than failing it.
func SupportedResourceTypes() []string {
	out := make([]string, 0, len(indexedTypes))
	for name := range indexedTypes {
		out = append(out, name)
	}
	return out
}

// countResponse is the count endpoint's body.
//
// Count is a pointer so that its absence is distinguishable from a count of
// zero. Decoded into a value it would make a malformed "{}" indistinguishable
// from "this project has none", which the sweep reports as a row still to be
// done — a wrong answer that looks like a right one, and the exact failure the
// error taxonomy exists to avoid.
//
// has_more is not required, and deliberately so: it means the count is a floor
// rather than exact, nothing here reads it, and the thresholds compared against
// are one or two — a floor that clears the minimum clears it. Rejecting a
// response for omitting a field that cannot change the answer would only invent
// a new way to fail.
type countResponse struct {
	Count   *uint64 `json:"count"`
	HasMore bool    `json:"has_more"`
}

// resourceEnvelope is the list endpoint's body. Only type and id are read.
//
// Resources is a pointer for the same reason Count is: a nil slice means the
// field was absent, which is a malformed response, while a present empty array
// is the documented race where the resource was deleted between the two calls.
// Those two get opposite treatment, so they cannot share a representation.
type resourceEnvelope struct {
	Resources *[]resourceRef `json:"resources"`
}

type resourceRef struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// Count reports how many resources of the given type the project has, and
// names one of them.
//
// Two calls, because the list endpoint returns no total and the count
// endpoint returns no identifiers. They carry identical filters, which is
// what makes the count a valid predicate for the list — the read layer
// filters both by the same principal, so a count of three followed by an
// empty list is a race rather than a disagreement.
//
// The list call is skipped entirely when the count is zero: there is nothing
// to name, and the row is not going to advance. The row's own min_count is
// compared in the service, not here, because the adapter's job ends at
// reporting what exists.
//
// No retries. The sweep is the retry — a pass that retried until it won would
// fight the person's edit that resolution deliberately yields to.
func (c *Client) Count(ctx context.Context, projectUID, resourceType string) (int, *model.ResolvedRef, error) {
	indexedType, ok := indexedTypes[resourceType]
	if !ok {
		// Not something this adapter can ask about. Distinct from finding
		// nothing: the caller should never have been given this lookup, so
		// it is a fault rather than a pending row.
		return 0, nil, fmt.Errorf("%w: no indexed type for resource type %q", domain.ErrInvalidRequest, resourceType)
	}
	if projectUID == "" {
		return 0, nil, fmt.Errorf("%w: empty project uid", domain.ErrInvalidRequest)
	}

	// One deadline across both calls. The http.Client's own timeout applies per
	// request, so without this a count answering just inside the bound followed
	// by a stalled reference call would hold the caller's row lock for twice
	// the configured timeout.
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	count, err := c.count(ctx, indexedType, projectUID)
	if err != nil {
		return 0, nil, err
	}
	if count == 0 {
		// The ordinary state of a row still to be done, and explicitly not an
		// error: reported as not-found so the pass counts it pending.
		return 0, nil, domain.ErrNotFound
	}

	ref, err := c.first(ctx, indexedType, projectUID)
	if err != nil {
		return 0, nil, err
	}
	if ref == nil {
		// Counted, then gone by the time we asked for it. A race between the
		// two calls, retried by the next sweep, and not a fault.
		return 0, nil, domain.ErrNotFound
	}
	return count, ref, nil
}

// count issues the aggregation call.
func (c *Client) count(ctx context.Context, indexedType, projectUID string) (int, error) {
	u := c.baseURL.JoinPath("query", "resources", "count")
	u.RawQuery = filters(indexedType, projectUID).Encode()

	var body countResponse
	if err := c.get(ctx, u, &body); err != nil {
		return 0, err
	}
	if body.Count == nil {
		return 0, errors.New("query service count response has no count field")
	}
	return int(*body.Count), nil
}

// first issues the list call and names the first hit.
//
// No ordering is imposed. The read layer's default sort is a total order, so
// the first hit is the same resource on every call while the underlying set
// is unchanged — imposing one here would only risk disagreeing with it.
func (c *Client) first(ctx context.Context, indexedType, projectUID string) (*model.ResolvedRef, error) {
	u := c.baseURL.JoinPath("query", "resources")
	q := filters(indexedType, projectUID)
	q.Set("page_size", "1")
	u.RawQuery = q.Encode()

	var body resourceEnvelope
	if err := c.get(ctx, u, &body); err != nil {
		return nil, err
	}
	if body.Resources == nil {
		return nil, errors.New("query service list response has no resources field")
	}
	if len(*body.Resources) == 0 {
		return nil, nil
	}
	hit := (*body.Resources)[0]
	if hit.ID == "" {
		return nil, nil
	}
	refType := hit.Type
	if refType == "" {
		refType = indexedType
	}
	return &model.ResolvedRef{Type: refType, UID: hit.ID}, nil
}

// filters builds the query parameters both calls share. Identical filters are
// the property the two-call shape depends on, so they are built once.
func filters(indexedType, projectUID string) url.Values {
	q := url.Values{}
	q.Set("v", apiVersion)
	q.Set("type", indexedType)
	q.Set("parent", "project:"+projectUID)
	return q
}

// get issues one request and decodes a 2xx body.
func (c *Client) get(ctx context.Context, u *url.URL, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("build query service request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		// Unreachable, refused, or timed out. A fault, so the sweep reports it
		// rather than recording every project as having done no work.
		return fmt.Errorf("query service request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		return statusError(resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		return fmt.Errorf("decode query service response: %w", err)
	}
	return nil
}

// statusError turns a non-2xx into an error that is deliberately never
// domain.ErrNotFound.
//
// This is the distinction the whole taxonomy exists for. An identity that has
// stopped being accepted returns 401 or 403 on every call; mapped to
// not-found it would read as every project having done no work, silently, on
// every sweep. Mapped to a fault it shows up in the sweep's failed count,
// which is the one signal that tells the two apart.
func statusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: query service rejected the service identity: status=%d body=%s",
			domain.ErrForbidden, resp.StatusCode, string(body))
	default:
		return fmt.Errorf("query service returned %s: body=%s",
			strconv.Itoa(resp.StatusCode), string(body))
	}
}
