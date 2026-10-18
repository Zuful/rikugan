// Package restclient provides the shared HTTP plumbing behind rikugan's
// REST-based providers.
//
// Every provider that talks to a JSON-over-HTTP API — Argo CD, PagerDuty,
// Sentry, Alertmanager, Jaeger, Loki, Falcosidekick — repeats the same
// sequence: build a GET request, attach static headers, reject anything
// that is not 200 with a readable error, then decode the body into a
// caller-supplied value. Client owns that sequence so each provider only
// declares what actually differs: the label it wears in error messages,
// its authentication headers, and any per-status guidance worth handing
// to the operator.
package restclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultTimeout is the request timeout applied when the caller does not
// supply its own *http.Client.
const DefaultTimeout = 30 * time.Second

// errorBodyLimit caps how much of a failed response body is echoed back in
// the error. Long enough to carry an API's own explanation, short enough
// to keep a tool result readable.
const errorBodyLimit = 1024

// Client performs JSON GET requests against a single upstream API.
//
// The zero value is not usable; build one with New.
type Client struct {
	name        string
	httpClient  *http.Client
	headers     map[string]string
	statusHints map[int]string
	secretURL   bool
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient replaces the underlying *http.Client. A nil argument is
// ignored, so callers may pass one through without a guard.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// WithTimeout overrides DefaultTimeout on the built-in *http.Client. It has
// no effect once WithHTTPClient has supplied a client of its own.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.httpClient.Timeout = d }
}

// WithHeader attaches a static header sent on every request — typically
// authentication or content negotiation.
func WithHeader(key, value string) Option {
	return func(c *Client) { c.headers[key] = value }
}

// WithStatusHint registers an operator-facing message for one HTTP status
// code. When the upstream answers with that code, the hint replaces the
// generic "returned N: body" error. Use it for the failures an operator can
// actually act on, such as a missing or expired token.
func WithStatusHint(code int, message string) Option {
	return func(c *Client) { c.statusHints[code] = message }
}

// WithSecretURL marks request URLs as credentials, as with a Telegram
// bot token in the path or a Slack incoming-webhook URL. Transport errors
// then name the failure without echoing the URL, which net/http otherwise
// puts in every error message and therefore in logs.
func WithSecretURL() Option {
	return func(c *Client) { c.secretURL = true }
}

// New returns a Client labelled name. That label is what appears in error
// messages, so it should read as the operator knows the system
// ("falcosidekick", not "falco").
func New(name string, opts ...Option) *Client {
	c := &Client{
		name:        name,
		httpClient:  &http.Client{Timeout: DefaultTimeout},
		headers:     make(map[string]string),
		statusHints: make(map[int]string),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// GetJSON issues a GET against rawURL and decodes a 200 response into dst.
// Any other status is an error: the registered hint for that code when one
// exists, otherwise the status plus a truncated copy of the response body.
func (c *Client) GetJSON(ctx context.Context, rawURL string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return c.requestError(err)
	}
	return c.do(req, func(code int) bool { return code == http.StatusOK }, dst)
}

// PostJSON encodes body as JSON, POSTs it to rawURL and, when dst is not
// nil, decodes a 2xx response into it. Errors follow GetJSON's rules.
func (c *Client) PostJSON(ctx context.Context, rawURL string, body, dst any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode %s request: %w", c.name, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(payload))
	if err != nil {
		return c.requestError(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, func(code int) bool { return code >= 200 && code < 300 }, dst)
}

// do sends req with the static headers, turns a status that accepted
// rejects into an error, and decodes the body into dst unless dst is nil.
func (c *Client) do(req *http.Request, accepted func(int) bool, dst any) error {
	for key, value := range c.headers {
		req.Header.Set(key, value)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s request failed: %w", c.name, c.redact(err))
	}
	defer func() { _ = resp.Body.Close() }()

	if !accepted(resp.StatusCode) {
		if hint, ok := c.statusHints[resp.StatusCode]; ok {
			return errors.New(hint)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		return fmt.Errorf("%s returned %d: %s", c.name, resp.StatusCode, string(body))
	}

	if dst == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return fmt.Errorf("decode %s response: %w", c.name, err)
	}
	return nil
}

// requestError reports a request that could not be built. With a secret
// URL the parse error, which quotes the URL, is not passed on.
func (c *Client) requestError(err error) error {
	if c.secretURL {
		return fmt.Errorf("create %s request: invalid URL", c.name)
	}
	return fmt.Errorf("create request: %w", err)
}

// redact strips the URL from a transport error when it is a credential.
func (c *Client) redact(err error) error {
	var urlErr *url.Error
	if c.secretURL && errors.As(err, &urlErr) {
		return fmt.Errorf("%s: %w", urlErr.Op, urlErr.Err)
	}
	return err
}

// JoinURL assembles a request URL from a base, a path and optional query
// parameters. Trailing slashes on base are dropped so callers need not
// normalise what they read from configuration.
func JoinURL(base, path string, params url.Values) string {
	u := strings.TrimRight(base, "/") + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	return u
}
