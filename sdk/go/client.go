// Package storvia is the Go client for the Storvia API.
//
// It shares the module path prefix with the service, which is convenient but not
// required: this package has no dependency on the server's internals, only on
// the published HTTP contract and the RFC 9457 error shape.
//
// The client deliberately mirrors the server's error vocabulary. An upload whose
// outcome is unknown comes back as KindUncertain rather than as a generic failure,
// because the correct client behaviour differs completely: retry the reconciliation
// call, never start a new upload elsewhere.
package storvia

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

// Kind is the machine-readable failure class returned by the API.
//
// The values match the server's problem types, so a caller can branch without
// parsing prose.
type Kind string

const (
	KindValidation            Kind = "validation"
	KindAuthentication        Kind = "authentication"
	KindAuthorization         Kind = "authorization"
	KindNotFound              Kind = "not_found"
	KindConflict              Kind = "conflict"
	KindQuota                 Kind = "quota"
	KindRateLimited           Kind = "rate_limited"
	KindUnavailable           Kind = "unavailable"
	KindUncertain             Kind = "uncertain"
	KindProviderNotConfigured Kind = "provider_not_configured"
	KindConsentRequired       Kind = "consent_required"
	KindInternal              Kind = "internal"
)

// Google OAuth scopes for Drive access.
const (
	// ScopeDriveFile is the narrow per-file scope. It lets Storvia access only
	// the files it creates or the user opens through Storvia.
	ScopeDriveFile = "https://www.googleapis.com/auth/drive.file"
	// ScopeDriveAppData is the app-data scope. It lets Storvia store files in
	// Drive's appDataFolder, which is visible only to Storvia.
	ScopeDriveAppData = "https://www.googleapis.com/auth/drive.appdata"
)

// problem is the RFC 9457 body the API returns for every failure.
type problem struct {
	Type   string            `json:"type"`
	Title  string            `json:"title"`
	Status int               `json:"status"`
	Detail string            `json:"detail"`
	Fields map[string]string `json:"fields,omitempty"`
}

// APIError is a failure returned by the API.
type APIError struct {
	Kind Kind
	// Status is the HTTP status code.
	Status int
	// Title and Detail are safe to show a user.
	Title  string
	Detail string
	// Fields carries structured context such as the offending field name.
	Fields map[string]string
	// RequestID correlates the failure with the server's logs. Quote it in a bug
	// report; it identifies the request without identifying the user.
	RequestID string
}

func (e *APIError) Error() string {
	if e.RequestID != "" {
		return fmt.Sprintf("storvia: %s: %s (request %s)", e.Kind, e.Detail, e.RequestID)
	}
	return fmt.Sprintf("storvia: %s: %s", e.Kind, e.Detail)
}

// Is allows errors.Is against a bare Kind.
func (e *APIError) Is(target error) bool {
	other, ok := target.(*APIError)
	return ok && other.Kind == e.Kind
}

// Temporary reports whether retrying the same call could succeed.
//
// It is deliberately false for quota, consent, and validation failures: retrying
// those just burns the user's quota or their patience.
func (e *APIError) Temporary() bool {
	switch e.Kind {
	case KindUnavailable, KindRateLimited, KindUncertain, KindInternal:
		return true
	default:
		return false
	}
}

// Config is the client configuration.
type Config struct {
	// BaseURL is the API root, for example https://api.storvia.example.
	BaseURL string
	// Token is the session credential. It may be empty when the client is used
	// from a browser that relies on the session cookie.
	Token string
	// HTTPClient is shared across calls so connections are reused.
	HTTPClient *http.Client
	// UserAgent identifies the caller in server logs.
	UserAgent string
	// Timeout bounds a single request. It must exceed the server's own budget.
	Timeout time.Duration
}

// Client is the Storvia API client.
type Client struct {
	baseURL    *url.URL
	token      string
	httpClient *http.Client
	userAgent  string
}

// New builds a client.
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("storvia: a base URL is required")
	}
	parsed, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("storvia: invalid base URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("storvia: base URL must be http or https, got %q", parsed.Scheme)
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		httpClient = &http.Client{Timeout: timeout}
	}
	userAgent := cfg.UserAgent
	if userAgent == "" {
		userAgent = "storvia-go-sdk"
	}

	return &Client{
		baseURL:    parsed,
		token:      cfg.Token,
		httpClient: httpClient,
		userAgent:  userAgent,
	}, nil
}

// BaseURL returns the configured API root.
func (c *Client) BaseURL() string { return c.baseURL.String() }

// HTTPClient returns the underlying HTTP client, for sub-packages that need to
// make raw provider requests (Drive resumable chunks, presigned parts).
func (c *Client) HTTPClient() *http.Client {
	return c.httpClient
}

// requestOption customizes one request.
type requestOption func(*http.Request)

// RequestOption is the public alias of requestOption, available to sub-packages.
type RequestOption = requestOption

// withIdempotencyKey attaches the key that makes a retried create safe.
//
// Without it, a browser retry creates a second provider write, which is the exact
// duplicate-file failure the API is built to prevent.
func withIdempotencyKey(key string) requestOption {
	return func(r *http.Request) {
		r.Header.Set("Idempotency-Key", key)
	}
}

// WithIdempotencyKey is the exported form of withIdempotencyKey.
var WithIdempotencyKey = withIdempotencyKey

// withBearer uses an Authorization header instead of the session cookie.
func withBearer(token string) requestOption {
	return func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+token)
	}
}

// WithBearer is the exported form of withBearer.
var WithBearer = withBearer

// do performs a JSON request and decodes the response.
func (c *Client) do(ctx context.Context, method, path string, body any, out any, opts ...requestOption) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("storvia: encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	return c.doRaw(ctx, method, path, reader, out, contentTypeJSON, opts...)
}

// DoRequest is the exported form of do, available to sub-packages and advanced
// callers who need to build their own request logic on top of the client.
func (c *Client) DoRequest(ctx context.Context, method, path string, body any, out any, opts ...RequestOption) error {
	return c.do(ctx, method, path, body, out, opts...)
}

// contentType values for the two body shapes.
const (
	contentTypeJSON   = "application/json"
	contentTypeBinary = "application/octet-stream"
)

// ContentTypeJSON is the exported form of contentTypeJSON.
var ContentTypeJSON = contentTypeJSON

// ContentTypeBinary is the exported form of contentTypeBinary.
var ContentTypeBinary = contentTypeBinary

// doRaw performs a request whose body is streamed rather than JSON encoded.
//
// Chunk uploads go through here: the bytes must not be buffered into a JSON
// document, and a large object must not be held in memory on its way out.
func (c *Client) doRaw(ctx context.Context, method, path string, body io.Reader, out any, contentType string, opts ...requestOption) error {

	endpoint := c.baseURL.JoinPath(path)
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return fmt.Errorf("storvia: build request: %w", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", contentType)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", c.userAgent)

	// A bearer token is set explicitly when the caller supplied one, and a cookie
	// jar on the HTTP client handles the browser case.
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	for _, opt := range opts {
		opt(request)
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		// A transport failure is an unknown outcome, not a confirmed failure: the
		// server may have committed the write before the response was lost.
		return &APIError{
			Kind:   KindUncertain,
			Status: 0,
			Title:  "Request could not be completed",
			Detail: "the request did not complete; the server may or may not have processed it",
		}
	}
	defer func() {
		// Drain a bounded amount so the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		_ = response.Body.Close()
	}()

	if response.StatusCode >= 400 {
		return decodeProblem(response)
	}
	if out == nil || response.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(out); err != nil {
		return fmt.Errorf("storvia: decode response: %w", err)
	}
	return nil
}

// decodeProblem turns an error response into an APIError.
func decodeProblem(response *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))

	var envelope struct {
		Error problem `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		// A non-JSON error body means something in front of the API failed. The
		// status is still useful, and the raw body is deliberately not surfaced.
		return &APIError{
			Kind:   kindForStatus(response.StatusCode),
			Status: response.StatusCode,
			Title:  http.StatusText(response.StatusCode),
			Detail: "the API returned a response that could not be parsed",
		}
	}

	return &APIError{
		Kind:      kindFromProblemType(envelope.Error.Type),
		Status:    envelope.Error.Status,
		Title:     envelope.Error.Title,
		Detail:    envelope.Error.Detail,
		Fields:    envelope.Error.Fields,
		RequestID: response.Header.Get("X-Request-ID"),
	}
}

// problemTypePrefix is the server's problem type namespace.
const problemTypePrefix = "https://storvia.dev/problems/"

// knownKinds is the set of kinds this SDK understands.
//
// The check is deliberate: a type registered by a newer server must not be
// returned verbatim as though this client knew its meaning, because a caller
// branching on an unrecognised kind would silently take the wrong action. Unknown
// types degrade to internal, which is the conservative choice.
var knownKinds = map[Kind]struct{}{
	KindValidation:            {},
	KindAuthentication:        {},
	KindAuthorization:         {},
	KindNotFound:              {},
	KindConflict:              {},
	KindQuota:                 {},
	KindRateLimited:           {},
	KindUnavailable:           {},
	KindUncertain:             {},
	KindProviderNotConfigured: {},
	KindConsentRequired:       {},
	KindInternal:              {},
}

// kindFromProblemType extracts the kind from the problem type URN.
func kindFromProblemType(value string) Kind {
	if !strings.HasPrefix(value, problemTypePrefix) {
		return KindInternal
	}
	kind := Kind(strings.TrimPrefix(value, problemTypePrefix))
	if _, ok := knownKinds[kind]; !ok {
		return KindInternal
	}
	return kind
}

// kindForStatus maps a status onto a kind when no problem type is available.
func kindForStatus(status int) Kind {
	switch {
	case status == http.StatusUnauthorized:
		return KindAuthentication
	case status == http.StatusForbidden:
		return KindAuthorization
	case status == http.StatusNotFound:
		return KindNotFound
	case status == http.StatusConflict:
		return KindConflict
	case status == http.StatusTooManyRequests:
		return KindRateLimited
	case status == http.StatusBadRequest, status == http.StatusUnprocessableEntity:
		return KindValidation
	case status == http.StatusServiceUnavailable:
		return KindUnavailable
	case status == http.StatusFailedDependency:
		return KindProviderNotConfigured
	case status == http.StatusPreconditionRequired:
		return KindConsentRequired
	default:
		return KindInternal
	}
}

// StatusCode extracts the HTTP status from an error, or 0 when it is not an
// APIError.
func StatusCode(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status
	}
	return 0
}

// IsKind reports whether err carries the given kind.
func IsKind(err error, kind Kind) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Kind == kind
	}
	return false
}

// formatBytes renders a byte count, used by the upload helpers' messages.
func formatBytes(count int64) string { return strconv.FormatInt(count, 10) }

// keep bytes imported for the request body construction above.
var _ = bytes.MinRead
