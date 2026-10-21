package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shovan04/storvia-project/internal/apperr"
	"github.com/shovan04/storvia-project/internal/auth"
	"github.com/shovan04/storvia-project/internal/logger"
)

// Middleware decorates an http.Handler.
type Middleware func(http.Handler) http.Handler

// Chain applies middleware so that the first argument is the outermost wrapper.
// Reading order matches execution order, which matters because the recovery
// middleware must be able to catch a panic from everything installed after it.
func Chain(handler http.Handler, middleware ...Middleware) http.Handler {
	for i := len(middleware) - 1; i >= 0; i-- {
		handler = middleware[i](handler)
	}
	return handler
}

// RequestID assigns or forwards a correlation ID and stamps it on the context.
//
// A caller-supplied ID is accepted so a trace survives across service hops, but
// it is length-limited and character-filtered before it reaches a log line, so a
// client cannot inject newlines or forge a convincing fake correlation.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := sanitizeRequestID(r.Header.Get("X-Request-ID"))
			if id == "" {
				id = newRequestID()
			}
			w.Header().Set("X-Request-ID", id)
			ctx := logger.WithCorrelationID(r.Context(), id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

const maxRequestIDLength = 64

func sanitizeRequestID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxRequestIDLength {
		return ""
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return ""
		}
	}
	return value
}

func newRequestID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(buf)
}

// statusRecorder captures the response status for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusRecorder) WriteHeader(status int) {
	s.status = status
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

// Flush forwards to the underlying writer so a streamed download is not buffered.
func (s *statusRecorder) Flush() {
	if flusher, ok := s.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Unwrap lets http.ResponseController reach the original writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// AccessLog emits one structured record per request.
//
// It logs method, path, status, duration, and size. It does not log query strings
// or bodies, because a presigned capability can arrive in a query parameter.
func AccessLog(base *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			recorder := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(recorder, r)

			status := recorder.status
			if status == 0 {
				status = http.StatusOK
			}
			logger.With(r.Context(), base).Info("http_request",
				"method", r.Method,
				// r.URL.Path only: the raw query is excluded on purpose.
				"path", r.URL.Path,
				"status", status,
				"duration_ms", time.Since(start).Milliseconds(),
				"bytes", recorder.bytes,
				"remote", clientIP(r),
			)
		})
	}
}

// clientIP reports the peer address. Proxy headers are ignored unless a trusted
// proxy is configured, because a client-supplied X-Forwarded-For is otherwise a
// trivial way to poison the access log.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Recovery converts a panic into a 500 without taking the process down.
//
// A panic in a handler is always an internal fault, so it is logged with a stack
// trace for the operator and reported to the client as a fixed message. A
// recovered panic during a provider write also leaves that upload in an unknown
// state, which the service resolves by reconciliation rather than by retrying
// elsewhere.
func Recovery() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if recovered := recover(); recovered != nil {
					if recovered == http.ErrAbortHandler {
						panic(recovered)
					}
					logger.With(r.Context(), slog.Default()).Error("httpapi: panic recovered",
						"panic", sanitizedPanic(recovered),
						"method", r.Method,
						"path", r.URL.Path,
						"stack", string(debug.Stack()),
					)
					WriteProblem(w, r, apperr.Internal("an unexpected error occurred"))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// sanitizedPanic renders a recovered value as a short string for the log.
func sanitizedPanic(value any) string {
	switch v := value.(type) {
	case error:
		return v.Error()
	case string:
		return v
	default:
		return "non-string panic value"
	}
}

// Timeout bounds handler execution.
//
// The deadline is the route's own budget, not the routing budget: a client
// streaming a large file needs minutes, while a metadata read needs seconds. A
// shared deadline is how a slow client upload starts looking like a Drive outage.
func Timeout(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// CORS applies a strict origin allow-list.
//
// Only configured origins are echoed. Credentials are permitted because the
// browser client authenticates with a session cookie, which is exactly why a
// wildcard origin is never allowed here.
func CORS(allowedOrigins []string) Middleware {
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, origin := range allowedOrigins {
		allowed[strings.TrimSpace(origin)] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" {
				if _, ok := allowed[origin]; ok {
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Set("Access-Control-Allow-Credentials", "true")
					w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
					w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Idempotency-Key, X-Request-ID")
					w.Header().Set("Access-Control-Expose-Headers", "X-Request-ID, ETag, Location")
					// Presigned capability URLs are single-use and short-lived, so
					// they must never be readable from a shared cache.
					w.Header().Set("Access-Control-Max-Age", "600")
					w.Header().Add("Vary", "Origin")
				}
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Authenticate resolves the caller and rejects unauthenticated requests.
//
// This is the boundary that makes tenant isolation structural: a handler that
// runs after this middleware can read the identity from the context, and no
// handler has any way to obtain an identity from the request body instead.
func Authenticate(issuer *auth.Issuer, sessions auth.Lookup) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := issuer.SessionCookieValue(r)
			if token == "" {
				token = auth.BearerToken(r)
			}
			if token == "" {
				WriteProblem(w, r, apperr.Authentication("a valid session is required"))
				return
			}

			verified, err := issuer.Verify(token, time.Now())
			if err != nil {
				WriteProblem(w, r, apperr.Authentication("the session is invalid or has expired"))
				return
			}

			identity, ok := sessions.LookupSession(r.Context(), verified.ID)
			if !ok {
				// The token verified but its session is gone, which is what a
				// revoked session looks like.
				WriteProblem(w, r, apperr.Authentication("the session is no longer active"))
				return
			}
			if !identity.Valid() {
				WriteProblem(w, r, apperr.Authentication("the session is not bound to a tenant"))
				return
			}
			if verified.ExpiresAt.Before(time.Now()) {
				WriteProblem(w, r, apperr.Authentication("the session has expired"))
				return
			}

			ctx := auth.WithIdentity(r.Context(), identity)
			ctx = logger.WithTenant(ctx, identity.TenantID, identity.UserID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RateLimiter is a per-key fixed-window limiter.
//
// It exists to bound the cost of a runaway or hostile client. The window is
// deliberately simple because the goal is a ceiling, not fairness.
type RateLimiter struct {
	mu       sync.Mutex
	windows  map[string]*window
	limit    int
	interval time.Duration
	// now is injectable so tests do not sleep.
	now func() time.Time
}

type window struct {
	count   int
	started time.Time
}

// NewRateLimiter builds a limiter allowing limit requests per interval per key.
func NewRateLimiter(limit int, interval time.Duration) *RateLimiter {
	return &RateLimiter{
		windows:  make(map[string]*window),
		limit:    limit,
		interval: interval,
		now:      time.Now,
	}
}

// Allow reports whether a request for key may proceed, and how long to wait when
// it may not.
func (l *RateLimiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	entry, ok := l.windows[key]
	if !ok || now.Sub(entry.started) >= l.interval {
		l.windows[key] = &window{count: 1, started: now}
		l.sweepLocked(now)
		return true, 0
	}
	if entry.count >= l.limit {
		return false, l.interval - now.Sub(entry.started)
	}
	entry.count++
	return true, 0
}

// sweepLocked drops expired windows so the map does not grow per client forever.
func (l *RateLimiter) sweepLocked(now time.Time) {
	if len(l.windows) < 1024 {
		return
	}
	for key, entry := range l.windows {
		if now.Sub(entry.started) >= l.interval {
			delete(l.windows, key)
		}
	}
}

// Middleware applies the limiter, using the tenant as the key so one tenant
// cannot exhaust another's budget.
func (l *RateLimiter) Middleware() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := clientIP(r)
			if identity, ok := auth.IdentityFrom(r.Context()); ok && identity.TenantID != "" {
				key = identity.TenantID + ":" + identity.UserID
			}
			allowed, retryAfter := l.Allow(key)
			if !allowed {
				w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
				WriteProblem(w, r, apperr.RateLimited("too many requests; retry after the indicated delay"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireTenant rejects a request whose session did not resolve a tenant. It is
// the last guard before a handler touches the metadata store.
func RequireTenant(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFrom(r.Context())
		if !ok || !identity.Valid() {
			WriteProblem(w, r, apperr.Authentication("the request is not bound to a tenant"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireIdempotencyKey enforces the header on endpoints that create state.
//
// Without a stable key a retried browser request would create a second provider
// write, which is the exact duplicate-file scenario this design refuses to allow.
func RequireIdempotencyKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" {
			WriteProblem(w, r, apperr.Validation("the Idempotency-Key header is required"))
			return
		}
		if len(key) > 255 {
			WriteProblem(w, r, apperr.Validation("the Idempotency-Key header is too long"))
			return
		}
		ctx := context.WithValue(r.Context(), idempotencyKeyContext{}, key)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type idempotencyKeyContext struct{}

// IdempotencyKey reads the validated key from the context.
func IdempotencyKey(ctx context.Context) string {
	key, _ := ctx.Value(idempotencyKeyContext{}).(string)
	return key
}
