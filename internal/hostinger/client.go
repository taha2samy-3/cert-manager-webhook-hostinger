package hostinger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the production endpoint of the Hostinger public API.
const DefaultBaseURL = "https://developers.hostinger.com"

const (
	defaultTimeout = 30 * time.Second
	maxRetries     = 3
	// maxRetryAfter caps how long we honour a Retry-After header so a single
	// challenge cannot block a webhook worker indefinitely.
	maxRetryAfter = 60 * time.Second
)

// RecordSet mirrors DNS.V1.Zone.RecordResource / the items of
// DNS.V1.Zone.UpdateRequest.zone in the Hostinger OpenAPI spec.
type RecordSet struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	TTL     int      `json:"ttl,omitempty"`
	Records []Record `json:"records"`
}

// Record mirrors DNS.V1.Zone.NameRecordResource.
type Record struct {
	Content    string `json:"content"`
	IsDisabled bool   `json:"is_disabled,omitempty"`
}

type updateRequest struct {
	Overwrite bool        `json:"overwrite"`
	Zone      []RecordSet `json:"zone"`
}

// DeleteFilter mirrors the items of DNS.V1.Zone.DestroyRequest.filters.
type DeleteFilter struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type deleteRequest struct {
	Filters []DeleteFilter `json:"filters"`
}

// APIError is returned for any non-2xx response from the Hostinger API.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("hostinger API returned %d: %s", e.StatusCode, e.Body)
}

// IsNotFound reports whether err is a 404 from the Hostinger API, which is
// what the zone endpoints return for a domain the token cannot manage.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// Client is a minimal client for the Hostinger DNS zone API.
type Client struct {
	baseURL    string
	token      string
	userAgent  string
	httpClient *http.Client
}

// NewClient returns a Client authenticating with the given bearer token.
func NewClient(baseURL, token, userAgent string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      token,
		userAgent:  userAgent,
		httpClient: &http.Client{Timeout: defaultTimeout},
	}
}

// GetRecords returns every record set in the zone of domain.
func (c *Client) GetRecords(ctx context.Context, domain string) ([]RecordSet, error) {
	var sets []RecordSet
	if err := c.do(ctx, http.MethodGet, zonePath(domain), nil, &sets); err != nil {
		return nil, err
	}
	return sets, nil
}

// UpdateRecords creates or updates record sets. With overwrite=false new
// records are appended to existing sets of the same name and type; with
// overwrite=true matching sets are replaced.
func (c *Client) UpdateRecords(ctx context.Context, domain string, overwrite bool, sets []RecordSet) error {
	return c.do(ctx, http.MethodPut, zonePath(domain), updateRequest{Overwrite: overwrite, Zone: sets}, nil)
}

// DeleteRecords removes every record set matching one of the filters.
func (c *Client) DeleteRecords(ctx context.Context, domain string, filters []DeleteFilter) error {
	return c.do(ctx, http.MethodDelete, zonePath(domain), deleteRequest{Filters: filters}, nil)
}

func zonePath(domain string) string {
	return "/api/dns/v1/zones/" + url.PathEscape(domain)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
	}

	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if c.userAgent != "" {
			req.Header.Set("User-Agent", c.userAgent)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("%s %s: %w", method, path, err)
		}
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if err != nil {
			return fmt.Errorf("reading response of %s %s: %w", method, path, err)
		}

		if resp.StatusCode == http.StatusTooManyRequests && attempt < maxRetries {
			if err := sleep(ctx, retryAfter(resp.Header.Get("Retry-After"), attempt)); err != nil {
				return err
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return &APIError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(respBody))}
		}
		if out != nil && len(respBody) > 0 {
			if err := json.Unmarshal(respBody, out); err != nil {
				return fmt.Errorf("decoding response of %s %s: %w", method, path, err)
			}
		}
		return nil
	}
}

// retryAfter parses a Retry-After header (seconds form, as documented by
// Hostinger) and falls back to exponential backoff when it is absent.
func retryAfter(header string, attempt int) time.Duration {
	d := time.Duration(1<<attempt) * time.Second
	if secs, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && secs >= 0 {
		d = time.Duration(secs) * time.Second
	}
	return min(d, maxRetryAfter)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
