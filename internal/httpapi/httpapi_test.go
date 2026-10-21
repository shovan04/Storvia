package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shovan04/storvia-project/internal/apperr"
	"github.com/shovan04/storvia-project/internal/auth"
)

// decodeProblem parses an error response body.
func decodeProblem(t *testing.T, body []byte) Problem {
	t.Helper()
	var envelope struct {
		Error Problem `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("error body is not valid JSON: %v (body: %s)", err, body)
	}
	return envelope.Error
}

// TestProblemTypeAndStatusMatchTheKind is the core guarantee of the global error
// handler: one kind, one status, one problem type, everywhere.
func TestProblemTypeAndStatusMatchTheKind(t *testing.T) {
	tests := []struct {
		kind   apperr.Kind
		status int
	}{
		{apperr.KindValidation, http.StatusBadRequest},
		{apperr.KindAuthentication, http.StatusUnauthorized},
		{apperr.KindAuthorization, http.StatusForbidden},
		{apperr.KindNotFound, http.StatusNotFound},
		{apperr.KindConflict, http.StatusConflict},
		{apperr.KindRateLimited, http.StatusTooManyRequests},
		{apperr.KindUnavailable, http.StatusServiceUnavailable},
		{apperr.KindUncertain, http.StatusConflict},
		{apperr.KindProviderNotConfigured, http.StatusFailedDependency},
		{apperr.KindConsentRequired, http.StatusPreconditionRequired},
		{apperr.KindInternal, http.StatusInternalServerError},
	}

	for _, tc := range tests {
		t.Run(string(tc.kind), func(t *testing.T) {
			err := apperr.New(tc.kind, "Title", "Detail")
			if got := err.Status(); got != tc.status {
				t.Errorf("Status = %d, want %d", got, tc.status)
			}
			want := "https://storvia.dev/problems/" + string(tc.kind)
			if got := err.ProblemType(); got != want {
				t.Errorf("ProblemType = %q, want %q", got, want)
			}
		})
	}
}

// TestWriteProblemNeverLeaksTheCause is the important one: an internal cause may
// appear in a log but must never reach the client.
func TestWriteProblemNeverLeaksTheCause(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/storage/objects/obj_1", nil)

	secret := "postgres://user:hunter2@db.internal:5432/storvia"
	cause := errors.New("connection failed: " + secret)
	err := apperr.Wrap(cause, apperr.KindInternal, "Internal error", "an unexpected error occurred")

	WriteProblem(recorder, request, err)

	body := recorder.Body.String()
	if strings.Contains(body, secret) {
		t.Errorf("the response leaked the internal cause: %s", body)
	}
	if strings.Contains(body, "hunter2") {
		t.Errorf("the response leaked a credential: %s", body)
	}

	problem := decodeProblem(t, recorder.Body.Bytes())
	if problem.Status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", problem.Status)
	}
	// A generic internal error must not describe internals to the caller.
	if problem.Detail != "an unexpected error occurred" {
		t.Errorf("detail = %q, want the generic message", problem.Detail)
	}
}

// TestWriteProblemCoercesForeignErrors makes an untyped error from a deep package
// safe: it becomes a 500 with a fixed body rather than its own message.
func TestWriteProblemCoercesForeignErrors(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil)

	WriteProblem(recorder, request, fmt.Errorf("some internal detail: %s", "secret-value"))

	body := recorder.Body.String()
	if strings.Contains(body, "secret-value") {
		t.Errorf("a foreign error's message reached the client: %s", body)
	}
	if recorder.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", recorder.Code)
	}
}

func TestWriteProblemHandlesNil(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil)

	WriteProblem(recorder, request, nil)
	if recorder.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 for a nil error", recorder.Code)
	}
}

func TestWriteProblemIncludesRequestCorrelation(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/storage/status", nil)
	ctx := WithCorrelationIDForTest(request.Context(), "corr-abc")
	request = request.WithContext(ctx)

	WriteProblem(recorder, request, apperr.NotFound("missing"))

	problem := decodeProblem(t, recorder.Body.Bytes())
	if problem.RequestID != "corr-abc" {
		t.Errorf("request_id = %q, want corr-abc", problem.RequestID)
	}
	if problem.Instance != "/v1/storage/status" {
		t.Errorf("instance = %q, want the request path", problem.Instance)
	}
}

func TestWriteProblemIncludesFields(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/storage/uploads", nil)

	err := apperr.Validation("size_bytes exceeds the limit").
		WithField("size_bytes", "maximum 1048576 bytes")

	WriteProblem(recorder, request, err)
	problem := decodeProblem(t, recorder.Body.Bytes())
	if problem.Fields["size_bytes"] == "" {
		t.Errorf("fields = %v, want the offending field", problem.Fields)
	}
}

func TestWithFieldDoesNotMutateTheOriginal(t *testing.T) {
	base := apperr.Validation("bad request")
	first := base.WithField("a", "1")
	second := base.WithField("b", "2")

	if len(base.Fields) != 0 {
		t.Errorf("the base error gained fields: %v", base.Fields)
	}
	if _, ok := first.Fields["b"]; ok {
		t.Error("WithField shares its map between copies")
	}
	if _, ok := second.Fields["a"]; ok {
		t.Error("WithField shares its map between copies")
	}
}

func TestErrorKindMatching(t *testing.T) {
	err := apperr.Quota("drive is full")
	if !apperr.Is(err, apperr.KindQuota) {
		t.Error("KindOf must recognize a quota error")
	}
	if apperr.Is(err, apperr.KindUnavailable) {
		t.Error("KindOf must not match the wrong kind")
	}
	// A nil error reports as internal, which is what makes it safe: there is no
	// kind for "nothing went wrong", so it lands in the non-revealing bucket.
	if apperr.KindOf(nil) != apperr.KindInternal {
		t.Errorf("KindOf(nil) = %q, want internal", apperr.KindOf(nil))
	}
	// An untyped error from a deep package also reports as internal rather than
	// carrying its own text to the client.
	if apperr.KindOf(fmt.Errorf("raw detail")) != apperr.KindInternal {
		t.Error("an untyped error must report as internal")
	}
	// A wrapped error keeps its kind through the chain.
	wrapped := fmt.Errorf("while uploading: %w", err)
	if !apperr.Is(wrapped, apperr.KindQuota) {
		t.Error("KindOf must follow the wrap chain")
	}
}

func TestDecodeJSONRejectsUnknownFields(t *testing.T) {
	// A misspelled "size_bytes" must not become a zero, which would mean an
	// unlimited upload.
	request := httptest.NewRequest(http.MethodPost, "/v1/storage/uploads",
		strings.NewReader(`{"name":"a.pdf","size_byte":100}`))
	recorder := httptest.NewRecorder()

	var body createUploadRequest
	err := DecodeJSON(recorder, request, &body)
	if err == nil {
		t.Fatal("an unknown field must be rejected")
	}
	if !apperr.Is(err, apperr.KindValidation) {
		t.Errorf("error kind = %q, want validation", apperr.KindOf(err))
	}
}

func TestDecodeJSONRejectsWrongTypes(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/storage/uploads",
		strings.NewReader(`{"name":"a.pdf","size_bytes":"not-a-number"}`))
	recorder := httptest.NewRecorder()

	var body createUploadRequest
	err := DecodeJSON(recorder, request, &body)
	if err == nil {
		t.Fatal("a wrong field type must be rejected")
	}
	// The offending field is reported structurally, not embedded in free text.
	var appErr *apperr.Error
	if !errors.As(err, &appErr) {
		t.Fatalf("error = %v, want an *apperr.Error", err)
	}
	if appErr.Fields["field"] != "size_bytes" {
		t.Errorf("fields = %v, want the offending field named", appErr.Fields)
	}
}

func TestDecodeJSONRejectsMalformedAndEmptyBodies(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"malformed", `{"name":`},
		{"empty", ``},
		{"not an object", `"a string"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/storage/uploads",
				strings.NewReader(tc.body))
			recorder := httptest.NewRecorder()

			var body createUploadRequest
			if err := DecodeJSON(recorder, request, &body); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestDecodeJSONRejectsWrongContentType(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/storage/uploads",
		strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "text/plain")
	recorder := httptest.NewRecorder()

	var body createUploadRequest
	if err := DecodeJSON(recorder, request, &body); err == nil {
		t.Fatal("a non-JSON content type must be rejected")
	}
}

func TestWriteJSONSetsSafeHeaders(t *testing.T) {
	recorder := httptest.NewRecorder()
	WriteJSON(recorder, http.StatusOK, map[string]string{"status": "ok"})

	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("Content-Type = %q, want JSON", got)
	}
	// User content must never be sniffed into an executable type.
	if got := recorder.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

func TestContentDispositionEscapesInjection(t *testing.T) {
	// A file name is user-controlled; an unescaped one is a header injection
	// vector.
	hostile := []string{
		`evil".html`,
		"evil\r\nX-Injected: 1",
		"evil\nSet-Cookie: a=b",
		"",
	}
	for _, name := range hostile {
		got := contentDisposition(name)
		if strings.ContainsAny(got, "\r\n") {
			t.Errorf("contentDisposition(%q) = %q, contains a newline", name, got)
		}
		if strings.Count(got, `"`) != 2 {
			t.Errorf("contentDisposition(%q) = %q, want exactly one quoted pair", name, got)
		}
	}

	if !strings.HasPrefix(contentDisposition("report.pdf"), "attachment;") {
		t.Errorf("contentDisposition = %q, want an attachment disposition", contentDisposition("report.pdf"))
	}
}

func TestContentRangeFormatting(t *testing.T) {
	byteRange, err := parseRangeForTest("bytes=0-99")
	if err != nil {
		t.Fatal(err)
	}
	if got := contentRange(byteRange, 1000); got != "bytes 0-99/1000" {
		t.Errorf("contentRange = %q, want bytes 0-99/1000", got)
	}

	// An open-ended range must be clamped to the object size.
	open, err := parseRangeForTest("bytes=900-")
	if err != nil {
		t.Fatal(err)
	}
	if got := contentRange(open, 1000); got != "bytes 900-999/1000" {
		t.Errorf("contentRange = %q, want bytes 900-999/1000", got)
	}
}

// parseRangeForTest is a thin wrapper so the test reads clearly.
func parseRangeForTest(header string) (*byteRangeType, error) {
	return parseRangeImpl(header)
}

// TestRateLimiterBlocksAfterTheLimit checks the per-identity ceiling.
func TestRateLimiterBlocksAfterTheLimit(t *testing.T) {
	limiter := NewRateLimiter(3, time.Minute)

	for i := 0; i < 3; i++ {
		if allowed, _ := limiter.Allow("tenant:user"); !allowed {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
	allowed, retryAfter := limiter.Allow("tenant:user")
	if allowed {
		t.Fatal("the fourth request must be blocked")
	}
	if retryAfter <= 0 {
		t.Errorf("retryAfter = %v, want a positive delay", retryAfter)
	}

	// A different identity has its own budget.
	if allowed, _ := limiter.Allow("tenant:other"); !allowed {
		t.Error("a different identity must have its own budget")
	}
}

func TestRateLimiterWindowResets(t *testing.T) {
	now := time.Now()
	limiter := NewRateLimiter(1, time.Minute)
	limiter.now = func() time.Time { return now }

	if allowed, _ := limiter.Allow("k"); !allowed {
		t.Fatal("the first request must be allowed")
	}
	if allowed, _ := limiter.Allow("k"); allowed {
		t.Fatal("the second request in the window must be blocked")
	}

	now = now.Add(2 * time.Minute)
	if allowed, _ := limiter.Allow("k"); !allowed {
		t.Error("a new window must reset the budget")
	}
}

func TestRequestIDIsSanitizedOrReplaced(t *testing.T) {
	tests := []struct {
		name       string
		supplied   string
		wantSameID bool
	}{
		{"clean id passes through", "abc-123_XYZ.1", true},
		{"injection attempt is replaced", "abc\r\nX-Evil: 1", false},
		{"overly long is replaced", strings.Repeat("a", 100), false},
		{"empty is replaced", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var captured string
			handler := RequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured = requestIDFromContext(r.Context())
			}))

			request := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.supplied != "" {
				request.Header.Set("X-Request-ID", tc.supplied)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)

			if captured == "" {
				t.Fatal("a correlation ID must always be set")
			}
			if tc.wantSameID && captured != tc.supplied {
				t.Errorf("id = %q, want the supplied %q", captured, tc.supplied)
			}
			if !tc.wantSameID && captured == tc.supplied && tc.supplied != "" {
				t.Errorf("id = %q, want a replacement", captured)
			}
			// The response header must match the context so a client can quote it.
			if got := recorder.Header().Get("X-Request-ID"); got != captured {
				t.Errorf("header id = %q, want %q", got, captured)
			}
		})
	}
}

func TestRecoveryConvertsPanicToInternalError(t *testing.T) {
	handler := Recovery()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("provider handler exploded: secret-token-abc123")
	}))

	request := httptest.NewRequest(http.MethodGet, "/v1/storage/uploads/upl_1/chunks", nil)
	recorder := httptest.NewRecorder()

	// A panic must not take the process down.
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "secret-token-abc123") {
		t.Errorf("the panic value leaked to the client: %s", recorder.Body.String())
	}
}

func TestRecoveryRethrowsHttpErrAbortHandler(t *testing.T) {
	// ErrAbortHandler is Go's signal that a client disconnected; swallowing it
	// would leave the server unable to shut down cleanly.
	handler := Recovery()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	defer func() {
		if recovered := recover(); recovered != http.ErrAbortHandler {
			t.Errorf("recovered %v, want http.ErrAbortHandler", recovered)
		}
	}()

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestCORSEchoesOnlyAllowedOrigins(t *testing.T) {
	handler := CORS([]string{"https://app.example.com"})(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

	// Allowed origin.
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Origin", "https://app.example.com")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("Allow-Origin = %q, want the configured origin", got)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Allow-Credentials = %q, want true for cookie auth", got)
	}

	// Disallowed origin: no header at all, never a wildcard.
	request = httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Origin", "https://evil.example.com")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q for a disallowed origin, want empty", got)
	}
}

func TestCORSHandlesPreflight(t *testing.T) {
	handler := CORS([]string{"https://app.example.com"})(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			t.Error("the handler must not run for a preflight")
		}))

	request := httptest.NewRequest(http.MethodOptions, "/v1/storage/uploads", nil)
	request.Header.Set("Origin", "https://app.example.com")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", recorder.Code)
	}
}

func TestRequireIdempotencyKey(t *testing.T) {
	var captured string
	handler := RequireIdempotencyKey(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = IdempotencyKey(r.Context())
	}))

	// Missing key is rejected before the handler runs.
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/storage/uploads", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 without an Idempotency-Key", recorder.Code)
	}
	if captured != "" {
		t.Error("the handler must not run without an idempotency key")
	}

	// Present key reaches the handler.
	request := httptest.NewRequest(http.MethodPost, "/v1/storage/uploads", nil)
	request.Header.Set("Idempotency-Key", "key-123")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if captured != "key-123" {
		t.Errorf("captured key = %q, want key-123", captured)
	}

	// An over-long key is rejected.
	request = httptest.NewRequest(http.MethodPost, "/v1/storage/uploads", nil)
	request.Header.Set("Idempotency-Key", strings.Repeat("k", 300))
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an over-long key", recorder.Code)
	}
}

// TestAuthenticateRejectsMissingAndInvalidSessions covers the gate every
// authenticated route depends on.
func TestAuthenticateRejectsMissingAndInvalidSessions(t *testing.T) {
	issuer, sessions := newTestAuth(t)
	handler := Authenticate(issuer, sessions)(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

	// No credential at all.
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/storage/status", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 with no credential", recorder.Code)
	}

	// A garbage token.
	request := httptest.NewRequest(http.MethodGet, "/v1/storage/status", nil)
	request.Header.Set("Authorization", "Bearer not-a-real-token")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for an invalid token", recorder.Code)
	}
}

func TestAuthenticatePassesIdentityIntoTheContext(t *testing.T) {
	issuer, sessions := newTestAuth(t)
	identity := auth.Identity{TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-1"}

	token, session, err := issuer.Issue(identity, "sess-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.StoreSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}

	var captured auth.Identity
	var capturedOK bool
	handler := Authenticate(issuer, sessions)(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			captured, capturedOK = auth.IdentityFrom(r.Context())
			w.WriteHeader(http.StatusOK)
		}))

	request := httptest.NewRequest(http.MethodGet, "/v1/storage/status", nil)
	request.AddCookie(&http.Cookie{Name: issuer.CookieName(), Value: token})
	handler.ServeHTTP(httptest.NewRecorder(), request)

	if !capturedOK {
		t.Fatal("the identity was not placed in the context")
	}
	// The identity must come from the session, never from the request path.
	if captured.TenantID != "tenant-a" || captured.UserID != "user-1" {
		t.Errorf("identity = %+v, want the session identity", captured)
	}
}

func TestAuthenticateRejectsARevokedSession(t *testing.T) {
	issuer, sessions := newTestAuth(t)
	identity := auth.Identity{TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-1"}

	token, session, err := issuer.Issue(identity, "sess-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.StoreSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	// Revoking the session: the token still verifies, but the session is gone.
	sessions.(*auth.MemorySessions).DeleteSession(context.Background(), "sess-1")

	handler := Authenticate(issuer, sessions)(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

	request := httptest.NewRequest(http.MethodGet, "/v1/storage/status", nil)
	request.AddCookie(&http.Cookie{Name: issuer.CookieName(), Value: token})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 after the session was revoked", recorder.Code)
	}
}

func TestRequireTenantBlocksARequestWithoutIdentity(t *testing.T) {
	called := false
	handler := RequireTenant(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/storage/status", nil))

	if called {
		t.Error("a request without an identity must not reach the handler")
	}
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", recorder.Code)
	}
}

func TestScopeFromReadsOnlyTheSession(t *testing.T) {
	identity := auth.Identity{TenantID: "tenant-a", UserID: "user-1"}
	ctx := auth.WithIdentity(context.Background(), identity)

	// Even with tenant-shaped values in the path and query, the scope must come
	// from the session.
	request := httptest.NewRequest(http.MethodGet,
		"/v1/storage/objects/tenant-b-user-2", nil).WithContext(ctx)

	scope, err := scopeFrom(request)
	if err != nil {
		t.Fatalf("scopeFrom: %v", err)
	}
	if scope.TenantID != "tenant-a" || scope.UserID != "user-1" {
		t.Errorf("scope = %+v, want the session tenant and user", scope)
	}
}

func TestAccessLogDoesNotLogQueryStrings(t *testing.T) {
	// A presigned capability can arrive in a query string; logging it would leak
	// a bearer token into the log store.
	var buf strings.Builder
	base := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	handler := AccessLog(base)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	request := httptest.NewRequest(http.MethodGet,
		"/v1/storage/objects/obj_1/content?token=SUPER-SECRET-CAPABILITY", nil)
	handler.ServeHTTP(httptest.NewRecorder(), request)

	if strings.Contains(buf.String(), "SUPER-SECRET-CAPABILITY") {
		t.Errorf("the access log captured a query string: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "/v1/storage/objects/obj_1/content") {
		t.Errorf("the access log should still record the path: %s", buf.String())
	}
}

func TestTimeoutMiddlewareCancelsTheContext(t *testing.T) {
	handler := Timeout(10 * time.Millisecond)(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			// The deadline must be visible to the handler, otherwise a slow client
			// upload can run unbounded.
			deadline, ok := r.Context().Deadline()
			if !ok {
				t.Error("no deadline on the request context")
				return
			}
			if time.Until(deadline) > time.Second {
				t.Errorf("deadline is %v away, want the configured 10ms", time.Until(deadline))
			}
			w.WriteHeader(http.StatusOK)
		}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

// newTestAuth builds an issuer and session store for the middleware tests.
func newTestAuth(t *testing.T) (*auth.Issuer, auth.Lookup) {
	t.Helper()
	issuer, err := auth.NewIssuer(auth.IssuerOptions{
		SigningKey: "middleware-test-signing-key-of-sufficient-length",
		CookieName: "storvia_session",
		TTL:        time.Hour,
	})
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	return issuer, auth.NewMemorySessions(100)
}

// WithCorrelationIDForTest and requestIDFromContext are thin helpers so the tests
// read without importing the logger package for two symbols.
func WithCorrelationIDForTest(ctx context.Context, id string) context.Context {
	return loggerWithCorrelationID(ctx, id)
}
