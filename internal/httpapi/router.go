package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/shovan04/storvia-project/internal/apperr"
	"github.com/shovan04/storvia-project/internal/auth"
	"github.com/shovan04/storvia-project/internal/logger"
	"github.com/shovan04/storvia-project/internal/persistence"
	"github.com/shovan04/storvia-project/internal/storage/service"
)

// Config is what the router needs to build the middleware stack and handlers.
type Config struct {
	Issuer   *auth.Issuer
	Sessions auth.Lookup
	Logger   *slog.Logger

	Uploads     *UploadHandler
	Objects     *ObjectHandler
	Connections *ConnectionHandler
	Consent     *ConsentHandler
	Console     *Console
	Health      *HealthHandler

	// AllowedOrigins is the CORS allow-list. An empty list disables CORS headers
	// entirely, which is the right default for a server-to-server deployment.
	AllowedOrigins []string
	// UploadTimeout and MetadataTimeout are separate budgets. Reusing one for both
	// is how a slow client upload starts looking like a provider outage.
	UploadTimeout   time.Duration
	MetadataTimeout time.Duration
	// RateLimit caps requests per interval per identity.
	RateLimit    int
	RateInterval time.Duration
}

// Router builds the HTTP handler.
//
// The split into a public mux and an authenticated mux is deliberate. Mounting a
// pattern on the authenticated mux means the middleware ordering is visible in
// one place, so nobody can add a route that reaches a handler without passing
// through authentication and the tenant check.
func Router(cfg Config) http.Handler {
	base := cfg.Logger
	if base == nil {
		base = slog.Default()
	}

	limiter := NewRateLimiter(cfg.RateLimit, cfg.RateInterval)

	// Global stack, outermost first. Recovery sits inside RequestID so a panic
	// still produces a correlated record, and outside everything that can panic.
	global := []Middleware{
		RequestID(),
		Recovery(),
		AccessLog(base),
		CORS(cfg.AllowedOrigins),
		limiter.Middleware(),
	}

	// wrapBounded applies a per-route deadline.
	wrapBounded := func(handler http.HandlerFunc, timeout time.Duration) http.HandlerFunc {
		return Chain(handler, Timeout(timeout)).ServeHTTP
	}

	// authenticated holds every route that requires a session. It is mounted
	// under /v1/ so that authentication is a property of the path prefix rather
	// than of each individual route.
	authenticated := http.NewServeMux()

	metadataRoutes := map[string]http.HandlerFunc{
		"GET /v1/storage/status":                          cfg.Uploads.Status,
		"GET /v1/storage/fallback/consent":                cfg.Consent.Show,
		"POST /v1/storage/fallback/consent":               cfg.Consent.Accept,
		"DELETE /v1/storage/fallback/consent":             cfg.Consent.Withdraw,
		"GET /v1/storage/connections/google-drive":        cfg.Connections.Show,
		"POST /v1/storage/connections/google-drive/start": cfg.Connections.Start,
		"DELETE /v1/storage/connections/google-drive":     cfg.Connections.Disconnect,
		"POST /v1/storage/uploads":                        cfg.Uploads.Create,
		"GET /v1/storage/uploads/{upload_id}":             cfg.Uploads.Get,
		"DELETE /v1/storage/uploads/{upload_id}":          cfg.Uploads.Cancel,
		"POST /v1/storage/uploads/{upload_id}/reconcile":  cfg.Uploads.Reconcile,
		"GET /v1/storage/objects/{object_id}":             cfg.Objects.Show,
		"GET /v1/storage/objects/{object_id}/content":     cfg.Objects.Download,
		"DELETE /v1/storage/objects/{object_id}":          cfg.Objects.Delete,
	}
	for pattern, handler := range metadataRoutes {
		authenticated.Handle(pattern, wrapBounded(handler, cfg.MetadataTimeout))
	}

	// Uploads carry file bytes, so they get the longer budget. An upload that
	// runs long is the client's pace, never evidence about Drive's health.
	uploadRoutes := map[string]http.HandlerFunc{
		"POST /v1/storage/uploads/{upload_id}/complete": cfg.Uploads.Complete,
	}
	for pattern, handler := range uploadRoutes {
		authenticated.Handle(pattern, wrapBounded(handler, cfg.UploadTimeout))
	}

	// The guard sits in front of the whole authenticated mux. RequireTenant is a
	// formality rather than the primary control; Authenticate is the control.
	guarded := Chain(authenticated, []Middleware{
		Authenticate(cfg.Issuer, cfg.Sessions),
		RequireTenant,
	}...)

	// Root mux. Public endpoints are listed explicitly; everything else that is
	// not a known public path goes through the guard.
	root := http.NewServeMux()
	root.HandleFunc("GET /{$}", rootHandler)
	root.Handle("GET /healthz", http.HandlerFunc(cfg.Health.Livez))
	root.Handle("GET /readyz", http.HandlerFunc(cfg.Health.Readyz))
	root.Handle("GET /metrics", http.HandlerFunc(cfg.Health.Metrics))
	// The OAuth callback is public: Google redirects the browser here without any
	// Storvia credential. The OAuth state value is what authenticates it.
	root.Handle("GET /v1/storage/connections/google-drive/callback", http.HandlerFunc(cfg.Connections.Callback))
	// The testing console. It replaces the earlier admin panel, which had grown a
	// set of form posts that could not show what an upload actually did.
	if cfg.Console != nil {
		// The static export first: it answers for documents and assets under /console,
		// and the more specific API and OAuth patterns below take precedence over it.
		mountConsole(root, consoleAssets)

		// The export is built with a trailing slash, so /console redirects rather than
		// serving the document at the slashless path where its relative asset URLs
		// would resolve one level too high.
		root.Handle("GET /console", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/console/", http.StatusMovedPermanently)
		}))

		// The OAuth callback is public: Google redirects here without any Storvia
		// credential, and the state value is what authenticates it.
		root.Handle("GET /console/oauth/callback", http.HandlerFunc(cfg.Console.Callback))
		root.Handle("GET /console/sign-in", http.HandlerFunc(cfg.Console.SignIn))

		// The OAuth callback is ALSO answered at the old /admin path, and this is not a
		// leftover of the old panel -- it is the only way sign-in can work.
		//
		// The callback URI is registered in Google Cloud, outside this repository, and
		// Google refuses a redirect_uri that is not registered. Verified directly against
		// accounts.google.com: an unregistered console URI comes back as
		// redirect_uri_mismatch before the user is ever shown a consent screen.
		//
		// So the registered URI has to stay the one Storvia sends, and the server has to
		// keep answering at it. Google redirects the browser here, this handler verifies
		// the state, exchanges the code, issues the session, sets the cookie, and then
		// hands the browser back to /console. The old path is where the trip *returns*,
		// not where the console lives.
		//
		// Nothing of the old panel is served here. /admin itself only redirects.
		root.Handle("GET /admin/oauth/callback", http.HandlerFunc(cfg.Console.Callback))

		// A stale bookmark or a link from an older changelog lands on the console rather
		// than a 404. A redirect is not the old site: there is nothing served here but a
		// pointer.
		root.Handle("GET /admin", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/console/", http.StatusMovedPermanently)
		}))
		root.Handle("GET /admin/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/console/", http.StatusMovedPermanently)
		}))

		console := func(h http.HandlerFunc) http.Handler {
			return Chain(h, RequestID(), AccessLog(cfg.Logger), Recovery(), limiter.Middleware())
		}
		for pattern, handler := range map[string]http.HandlerFunc{
			"GET /console/api/state":                       cfg.Console.State,
			"POST /console/api/upload":                     cfg.Console.Upload,
			"POST /console/api/routing":                    cfg.Console.SetRouting,
			"POST /console/api/faults":                     cfg.Console.SetFaults,
			"POST /console/api/consent":                    cfg.Console.SetConsent,
			"POST /console/api/folder":                     cfg.Console.EnsureFolder,
			"POST /console/api/disconnect":                 cfg.Console.Disconnect,
			"GET /console/api/objects/{object_id}/content": cfg.Console.DownloadContent,
			"DELETE /console/api/objects":                  cfg.Console.DeleteAllObjects,
			"DELETE /console/api/objects/{object_id}":      cfg.Console.DeleteObject,
		} {
			root.Handle(pattern, console(handler))
		}
	}
	root.Handle("/v1/", guarded)

	// Anything else gets a 404 in the same error format as everything else, so a
	// client never has to parse two different error shapes.
	notFound := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteProblem(w, r, apperr.NotFound("no route matches this request"))
	})
	root.Handle("/", notFound)

	return Chain(root, global...)
}

// rootHandler describes the service without exposing configuration.
func rootHandler(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, map[string]any{
		"service": "storvia",
		"version": "v1",
	})
}

// scopeFrom resolves the persistence scope for the request.
//
// The scope is built only from the verified session. There is deliberately no
// path or body field that contributes to it, which is what makes cross-tenant
// access impossible rather than merely discouraged.
func scopeFrom(r *http.Request) (persistence.Scope, error) {
	identity, ok := auth.IdentityFrom(r.Context())
	if !ok || !identity.Valid() {
		return persistence.Scope{}, apperr.Authentication("the request is not bound to a tenant")
	}
	return persistence.Scope{TenantID: identity.TenantID, UserID: identity.UserID}, nil
}

// loggingContext returns a logger carrying the request's correlation data.
func loggingContext(ctx context.Context, base *slog.Logger) *slog.Logger {
	return logger.With(ctx, base)
}

// interface guard: the service must keep satisfying the coordinator contract the
// handlers depend on.
var _ = (*service.UploadService)(nil)
