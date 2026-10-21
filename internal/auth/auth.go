// Package auth resolves the application identity used for every Storvia request.
//
// This is deliberately separate from Google Drive authorization. The application
// session decides who the caller is and which tenant they belong to; the Drive
// connection decides what that user may do with their own Drive. Neither is ever
// inferred from a request parameter.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/shovan04/storvia-project/internal/apperr"
)

// Identity is the authenticated caller.
//
// TenantID and UserID are resolved from a verified session and are the only
// accepted authority for any storage operation. A request body, query parameter,
// or provider identifier can never supply them.
type Identity struct {
	TenantID  string
	UserID    string
	SessionID string
	// Email is informational for logging. It is never an authorization input and
	// is not written to storage rows.
	Email string
}

// Valid reports whether the identity is usable.
func (i Identity) Valid() bool { return i.TenantID != "" && i.UserID != "" }

type contextKey struct{}

// ErrInvalidSession is the sentinel for a missing or unverifiable session.
var ErrInvalidSession = errors.New("auth: invalid session")

// WithIdentity stores the identity in the context.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

// IdentityFrom reads the identity from the context.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(contextKey{}).(Identity)
	return id, ok
}

// Issuer signs and verifies opaque session tokens.
//
// The token is HMAC-SHA256 over an opaque session identifier, not over a
// user-supplied payload, so no identity claim can be edited by the holder. The
// signing key comes from the environment.
type Issuer struct {
	key    []byte
	cookie string
	ttl    time.Duration
	secure bool
}

// IssuerOptions configures Issuer.
type IssuerOptions struct {
	// SigningKey is the HMAC key. It must come from the environment.
	SigningKey string
	CookieName string
	TTL        time.Duration
	// Secure marks the cookie Secure. It must be true outside development.
	Secure bool
}

// NewIssuer builds an Issuer.
func NewIssuer(opts IssuerOptions) (*Issuer, error) {
	if len(opts.SigningKey) < 32 {
		// A short HMAC key is forgeable by brute force, so this is a hard stop
		// rather than a warning.
		return nil, errors.New("auth: signing key must be at least 32 characters")
	}
	if opts.CookieName == "" {
		opts.CookieName = "storvia_session"
	}
	if opts.TTL <= 0 {
		opts.TTL = 12 * time.Hour
	}
	return &Issuer{
		key:    []byte(opts.SigningKey),
		cookie: opts.CookieName,
		ttl:    opts.TTL,
		secure: opts.Secure,
	}, nil
}

// CookieName returns the session cookie name.
func (i *Issuer) CookieName() string { return i.cookie }

// sign returns the HMAC of the session id.
func (i *Issuer) sign(sessionID string) string {
	mac := hmac.New(sha256.New, i.key)
	mac.Write([]byte(sessionID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Session is a verified application session.
type Session struct {
	ID        string
	Identity  Identity
	ExpiresAt time.Time
}

// token is the wire format: <sessionID>.<expiryUnix>.<signature>
func (i *Issuer) token(session Session) string {
	payload := fmt.Sprintf("%s.%d", session.ID, session.ExpiresAt.Unix())
	return payload + "." + i.sign(payload)
}

// Issue mints a signed session token.
func (i *Issuer) Issue(id Identity, sessionID string, ttl time.Duration) (string, Session, error) {
	if !id.Valid() {
		return "", Session{}, apperr.Internal("cannot issue a session without a tenant and user")
	}
	if ttl <= 0 {
		ttl = i.ttl
	}
	if sessionID == "" {
		generated, err := randomToken(16)
		if err != nil {
			return "", Session{}, err
		}
		sessionID = generated
	}
	session := Session{
		ID:        sessionID,
		Identity:  id,
		ExpiresAt: time.Now().Add(ttl).UTC(),
	}
	return i.token(session), session, nil
}

// Verify checks a token's signature and expiry.
//
// The comparison is constant time. A token that fails any check is reported
// identically, so a caller cannot distinguish "bad signature" from "expired" and
// use the difference to probe the token space.
func (i *Issuer) Verify(token string, now time.Time) (Session, error) {
	// The token is <sessionID>.<expiry>.<signature>, and the signature covers the
	// first two segments joined by a dot. Splitting on the last dot is what makes
	// that unambiguous; cutting on the first would swallow the expiry into the
	// signature and reject every genuine token.
	dot := strings.LastIndex(token, ".")
	if dot <= 0 || dot == len(token)-1 {
		return Session{}, ErrInvalidSession
	}
	payload, signature := token[:dot], token[dot+1:]
	if !hmac.Equal([]byte(signature), []byte(i.sign(payload))) {
		return Session{}, ErrInvalidSession
	}
	sessionID, expiryText, found := strings.Cut(payload, ".")
	if !found {
		return Session{}, ErrInvalidSession
	}
	expiryUnix, err := parseUnix(expiryText)
	if err != nil {
		return Session{}, ErrInvalidSession
	}
	if now.After(time.Unix(expiryUnix, 0)) {
		return Session{}, ErrInvalidSession
	}
	return Session{ID: sessionID, ExpiresAt: time.Unix(expiryUnix, 0).UTC()}, nil
}

// Lookup resolves a session ID to its identity.
//
// This is the seam where a shared session store would sit. The in-memory
// implementation is the development default; a production deployment points it at
// a durable store so a restart does not sign every user out.
type Lookup interface {
	// LookupSession returns the identity bound to a session ID.
	LookupSession(ctx context.Context, sessionID string) (Identity, bool)
	// StoreSession records a session so Verify can resolve its identity later.
	StoreSession(ctx context.Context, session Session) error
}

// randomToken returns a URL-safe random identifier.
func randomToken(bytes int) (string, error) {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// constantTimeEqual compares two strings without leaking their contents by
// timing. It returns false for length mismatches, which is safe because the
// lengths here are both fixed by the HMAC.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// SetSessionCookie writes the session cookie with conservative attributes.
//
// MaxAge is set alongside Expires, not instead of it. Browsers honour both, but
// MaxAge is the more widely and reliably implemented of the two, and a cookie that
// some browsers treat as a session cookie disappears when the browser closes. The
// operator expectation here is explicit -- stay signed in until signing out -- so the
// cookie is given both.
func (i *Issuer) SetSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:  i.cookie,
		Value: token,
		Path:  "/",
		// MaxAge is truncated to whole seconds because the header is an integer, and
		// a sub-second remainder truncating to zero would produce a cookie that
		// expires immediately.
		MaxAge:   int(time.Until(expires).Seconds()),
		Expires:  expires,
		HttpOnly: true,
		Secure:   i.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearSessionCookie expires the session cookie.
func (i *Issuer) ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     i.cookie,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   i.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// SessionCookieValue extracts the session token from a request.
func (i *Issuer) SessionCookieValue(r *http.Request) string {
	cookie, err := r.Cookie(i.cookie)
	if err != nil {
		return ""
	}
	return cookie.Value
}

// BearerToken extracts a bearer credential from the Authorization header. The
// SDK uses it for server-to-server callers; browser clients use the cookie.
func BearerToken(r *http.Request) string {
	const prefix = "bearer "
	header := r.Header.Get("Authorization")
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}
