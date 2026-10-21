package httpapi

import (
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"net/http"
	"time"

	"github.com/shovan04/storvia-project/internal/auth"
	"github.com/shovan04/storvia-project/internal/persistence"
	"github.com/shovan04/storvia-project/internal/storage/service"
)

// ConsoleOptions assembles the console.
type ConsoleOptions struct {
	Issuer      *auth.Issuer
	Sessions    auth.Lookup
	Uploads     *service.UploadService
	Objects     *service.ObjectService
	Connections *service.ConnectionService
	Consent     ConsentController
	Diagnostics DiagnosticsProvider
	Folders     FolderEnsurer
	Objects_    *persistence.ObjectRepository
	Routing     RoutingControl
	Faults      *FaultSwitch
	Logger      *slog.Logger
}

// NewConsole builds the testing console.
//
// The two HTTP clients are separate because their timeout profiles differ. Metadata
// calls must not hang for minutes, and an upload of half a gigabyte must not be cut
// off at a few seconds -- a timeout here would look exactly like a provider failure.
func NewConsole(cfg AdminPanelConfig, opts ConsoleOptions) *Console {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Console{
		cfg:         cfg,
		issuer:      opts.Issuer,
		sessions:    opts.Sessions,
		uploads:     opts.Uploads,
		objects:     opts.Objects,
		connects:    opts.Connections,
		consent:     opts.Consent,
		diagnostics: opts.Diagnostics,
		folders:     opts.Folders,
		objects_:    opts.Objects_,
		routing:     opts.Routing,
		faults:      opts.Faults,
		log:         log,

		// No overall timeout on the transfer: an upload's duration is a function of
		// how fast the operator's connection is, and cutting it off would manufacture
		// the failure the panel exists to detect.
		driveHTTPClient:    &http.Client{},
		fallbackHTTPClient: &http.Client{},
	}
}

// beginConnect starts the Google OAuth flow for the console.
//
// It reuses the same connection service the product API uses, so the console
// exercises the real OAuth path rather than a second implementation of it.
func (c *Console) beginConnect(w http.ResponseWriter, r *http.Request) (string, error) {
	flow, err := randomAdminToken()
	if err != nil {
		return "", err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     c.cfg.FlowCookie,
		Value:    flow,
		Path:     "/",
		MaxAge:   int((10 * time.Minute).Seconds()),
		HttpOnly: true,
		Secure:   c.cfg.Secure,
		SameSite: http.SameSiteLaxMode,
	})
	return c.connects.BeginConnectAt(r.Context(), c.scopeFor(), flow, c.cfg.RedirectURI)
}

// completeConnect exchanges the Google code, stores the connection, and issues the
// console session.
func (c *Console) completeConnect(w http.ResponseWriter, r *http.Request) error {
	code := r.URL.Query().Get("code")
	if code == "" {
		return errMissingCode
	}
	flowCookie, err := r.Cookie(c.cfg.FlowCookie)
	if err != nil || flowCookie.Value == "" {
		return errMissingFlowCookie
	}
	// The cookie is cleared as soon as it is read: a state value that can be replayed
	// is a state value that can be stolen.
	c.clearFlowCookie(w)

	if err := c.connects.CompleteConnectAt(r.Context(), c.scopeFor(), code,
		r.URL.Query().Get("state"), flowCookie.Value, c.cfg.RedirectURI); err != nil {
		return err
	}

	identity := auth.Identity{
		TenantID: c.cfg.TenantID,
		UserID:   c.cfg.UserID,
		Email:    "console-operator",
	}
	ttl := c.cfg.AdminSessionTTL
	if ttl <= 0 {
		ttl = c.cfg.SessionTTL
	}
	token, session, err := c.issuer.Issue(identity, "", ttl)
	if err != nil {
		return err
	}
	if err := c.sessions.StoreSession(r.Context(), session); err != nil {
		return err
	}
	c.issuer.SetSessionCookie(w, token, session.ExpiresAt)
	return nil
}

func (c *Console) clearFlowCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     c.cfg.FlowCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   c.cfg.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// randomAdminToken mints the OAuth flow value.
//
// It is 32 bytes of randomness: this value is what ties a callback to the browser
// that started the flow, so it must not be guessable by anyone who can trigger a
// sign-in of their own.
func randomAdminToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// issuerOrTest returns the console's issuer.
//
// Tests need to mint a session to exercise the authenticated routes, and the issuer is
// the only thing that can produce a token the console will accept.
func (c *Console) issuerOrTest() *auth.Issuer {
	if c.issuer == nil {
		panic("httpapi: the console has no session issuer")
	}
	return c.issuer
}
