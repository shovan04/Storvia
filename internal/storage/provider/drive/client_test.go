package drive

import (
	"context"
	urlpkg "net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shovan04/storvia-project/internal/persistence"
	"github.com/shovan04/storvia-project/internal/storage/model"
	"github.com/shovan04/storvia-project/internal/storage/policy"
)

// newTestClient builds a Drive client with no credentials.
func newTestClient(t *testing.T, configure bool) *Client {
	t.Helper()

	credentials := Credentials{}
	if configure {
		credentials = Credentials{
			ClientID:     "client-id.apps.googleusercontent.com",
			ClientSecret: "client-secret",
			RedirectURI:  "http://127.0.0.1:8080/v1/storage/connections/google-drive/callback",
		}
	}

	client, err := New(context.Background(), Config{
		Credentials: credentials,
		Scopes:      []string{ScopeDriveFile},
	}, ClientDeps{
		Tokens:   NewMemoryTokens(),
		Resolver: &stubResolver{},
		States:   NewStateStore(time.Minute),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

// stubResolver is a ConnectionResolver stub.
type stubResolver struct {
	connection   Connection
	err          error
	saved        []Connection
	markedStatus []string
	// The profile fields the last SaveConnection carried, so a test can assert that
	// what Google reported about the account actually reached storage.
	email       string
	displayName string
	picture     string
}

func (s *stubResolver) Connection(_ context.Context, _ persistence.Scope) (Connection, error) {
	if s.err != nil {
		return Connection{}, s.err
	}
	return s.connection, nil
}

func (s *stubResolver) SaveConnection(_ context.Context, c Connection) error {
	s.email, s.displayName, s.picture = c.Email, c.DisplayName, c.PictureURL
	s.saved = append(s.saved, c)
	return nil
}

func (s *stubResolver) ConnectionByID(_ context.Context, id string) (Connection, error) {
	if s.err != nil {
		return Connection{}, s.err
	}
	connection := s.connection
	connection.ID = id
	return connection, nil
}

func (s *stubResolver) SetChangesCursor(_ context.Context, _, cursor string) error {
	value := cursor
	s.connection.ChangesCursor = &value
	return nil
}

// AccountProfile is display-only, so a stub that returns nothing is a faithful
// "connected before the profile existed" rather than a special case.
func (s *stubResolver) AccountProfile(_ context.Context, _ persistence.Scope) (persistence.ConnectionProfile, error) {
	return persistence.ConnectionProfile{
		Email:       s.email,
		DisplayName: s.displayName,
		PictureURL:  s.picture,
	}, nil
}

func (s *stubResolver) SaveAccountProfile(_ context.Context, _ persistence.Scope, profile persistence.ConnectionProfile) error {
	s.email, s.displayName, s.picture = profile.Email, profile.DisplayName, profile.PictureURL
	return nil
}

func (s *stubResolver) MarkStatus(_ context.Context, _ string, status string) error {
	s.markedStatus = append(s.markedStatus, status)
	return nil
}

// TestScopeIsRestrictedToDriveFile is the single most consequential
// configuration check in this project.
func TestScopeIsRestrictedToDriveFile(t *testing.T) {
	_, err := New(context.Background(), Config{
		Credentials: Credentials{ClientID: "id", ClientSecret: "secret"},
		Scopes:      []string{"https://www.googleapis.com/auth/drive"},
	}, ClientDeps{Tokens: NewMemoryTokens(), Resolver: &stubResolver{}})
	if err == nil {
		t.Fatal("the full drive scope must be rejected")
	}
	// The message must name the offending scope and the permitted one, so an
	// operator can fix the config without reading the source.
	if !strings.Contains(err.Error(), "not permitted") {
		t.Errorf("error = %v, want it to explain the restriction", err)
	}
}

func TestScopesDefaultToDriveFile(t *testing.T) {
	client := newTestClient(t, false)
	if len(client.cfg.Scopes) != 1 || client.cfg.Scopes[0] != ScopeDriveFile {
		t.Errorf("scopes = %v, want only drive.file", client.cfg.Scopes)
	}
}

func TestConfiguredReflectsCredentials(t *testing.T) {
	if newTestClient(t, false).Configured() {
		t.Error("a client without credentials must report not configured")
	}
	if !newTestClient(t, true).Configured() {
		t.Error("a client with credentials must report configured")
	}
}

// TestAvailabilityWithoutCredentialsIsAConfigurationError is the guard that stops
// a missing deployment config from looking like a Drive outage, which would route
// every new upload into the fallback bucket on the first request.
func TestAvailabilityWithoutCredentialsIsAConfigurationError(t *testing.T) {
	client := newTestClient(t, false)

	err := client.Availability(context.Background(), model.AvailabilityScope{})
	var providerErr *policy.ProviderError
	if !errorsAs(err, &providerErr) {
		t.Fatalf("error = %v, want a *policy.ProviderError", err)
	}
	if providerErr.Class != policy.ClassConfiguration {
		t.Errorf("class = %q, want configuration; a missing credential is not an outage", providerErr.Class)
	}
	if policy.EligibleForFallbackForError(providerErr, policy.PhasePreflight) {
		t.Error("a configuration failure must never be eligible for the fallback")
	}
}

func TestAvailabilityWithABrokenConnectionIsConsentRequired(t *testing.T) {
	client := newTestClient(t, true)
	client.conns = &stubResolver{
		connection: Connection{ID: "dc-1", Status: StatusDisconnected},
	}

	err := client.Availability(context.Background(), model.AvailabilityScope{})
	var providerErr *policy.ProviderError
	if !errorsAs(err, &providerErr) {
		t.Fatalf("error = %v, want a *policy.ProviderError", err)
	}
	if providerErr.Class != policy.ClassConsentRequired {
		t.Errorf("class = %q, want consent_required", providerErr.Class)
	}
	if policy.EligibleForFallbackForError(providerErr, policy.PhasePreflight) {
		t.Error("a disconnected account must never reach the fallback")
	}
}

func TestClassifyGoogleStatusThroughNormalize(t *testing.T) {
	client := newTestClient(t, true)

	tests := []struct {
		name   string
		status int
		reason string
		want   policy.FailureClass
	}{
		{"quota", 403, "storageQuotaExceeded", policy.ClassQuota},
		{"admin policy", 403, "domainPolicy", policy.ClassAuthorization},
		{"rate limited", 429, "rateLimitExceeded", policy.ClassRateLimited},
		{"missing", 404, "notFound", policy.ClassMissing},
		{"server error", 503, "", policy.ClassUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"error":{"errors":[{"reason":"` + tc.reason + `"}],"status":"` + tc.reason + `"}}`)
			err := client.normalizeStatus("Availability", responseWithBody(tc.status, body))

			var providerErr *policy.ProviderError
			if !errorsAs(err, &providerErr) {
				t.Fatalf("error = %v", err)
			}
			if providerErr.Class != tc.want {
				t.Errorf("class = %q, want %q", providerErr.Class, tc.want)
			}
			if providerErr.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", providerErr.StatusCode, tc.status)
			}
		})
	}
}

// TestTransportErrorIsAvailability keeps a network failure classified as an
// outage, which is what makes the read-only preflight useful.
func TestTransportErrorIsAvailability(t *testing.T) {
	client := newTestClient(t, true)

	err := client.normalizeTransportError("Availability", context.DeadlineExceeded)
	var providerErr *policy.ProviderError
	if !errorsAs(err, &providerErr) {
		t.Fatalf("error = %v", err)
	}
	if providerErr.Class != policy.ClassUnavailable {
		t.Errorf("class = %q, want unavailable", providerErr.Class)
	}
	if !providerErr.Retryable {
		t.Error("a transport failure is retryable")
	}
	if !policy.EligibleForFallbackForError(providerErr, policy.PhasePreflight) {
		t.Error("a preflight transport failure must be eligible for the fallback")
	}
}

// TestStateStoreIsSingleUse is the OAuth CSRF guard.
func TestStateStoreIsSingleUse(t *testing.T) {
	store := NewStateStore(time.Minute)

	store.Put("state-1", "browser-a")
	if !store.Consume("state-1", "browser-a") {
		t.Fatal("the first consume must succeed")
	}
	// A replay must fail even though the value was valid a moment ago.
	if store.Consume("state-1", "browser-a") {
		t.Error("a state value must be single use; a replay would let an attacker redeem a code")
	}
}

func TestStateStoreBindsToTheBrowserSession(t *testing.T) {
	store := NewStateStore(time.Minute)

	store.Put("state-1", "browser-a")
	// The wrong browser must be rejected, and the attempt must burn the state.
	if store.Consume("state-1", "browser-b") {
		t.Error("a state bound to another browser must not verify")
	}
	if store.Consume("state-1", "browser-a") {
		t.Error("a failed binding check must not leave the state usable")
	}
}

func TestStateStoreRejectsUnknownValues(t *testing.T) {
	store := NewStateStore(time.Minute)
	if store.Consume("never-issued", "browser-a") {
		t.Error("an unknown state must not verify")
	}
}

func TestStateStoreExpires(t *testing.T) {
	now := time.Now()
	store := NewStateStore(time.Minute)
	store.now = func() time.Time { return now }

	store.Put("state-1", "browser-a")
	now = now.Add(2 * time.Minute)

	if store.Consume("state-1", "browser-a") {
		t.Error("an expired state must not verify; the user may never finish consent")
	}
}

func TestStateStoreSweepsExpiredEntries(t *testing.T) {
	now := time.Now()
	store := NewStateStore(time.Minute)
	store.now = func() time.Time { return now }

	for i := 0; i < 5; i++ {
		store.Put(string(rune('a'+i)), "browser-a")
	}
	if store.Len() != 5 {
		t.Errorf("Len = %d, want 5", store.Len())
	}

	now = now.Add(2 * time.Minute)
	if store.Len() != 0 {
		t.Errorf("Len = %d after expiry, want 0; a long-lived process must not accumulate them", store.Len())
	}
}

func TestAuthorizeURLCarriesTheRequiredParameters(t *testing.T) {
	client := newTestClient(t, true)

	url, err := client.AuthorizeURL("state-123", "browser-binding")
	if err != nil {
		t.Fatalf("AuthorizeURL: %v", err)
	}

	// offline access is required for a backend that acts while the user is away.
	if !strings.Contains(url, "access_type=offline") {
		t.Error("access_type=offline is required to obtain a refresh token")
	}
	if !strings.Contains(url, "response_type=code") {
		t.Error("response_type=code is required")
	}
	if !strings.Contains(url, "drive.file") {
		t.Error("the narrow scope must be requested")
	}
	// The redirect URI must match exactly, including a trailing slash.
	if !strings.Contains(url, "google-drive%2Fcallback") {
		t.Errorf("the redirect URI must be sent exactly: %s", url)
	}
	if !strings.Contains(url, "state=state-123") {
		t.Error("the state must be present")
	}
	parsed, err := urlpkg.Parse(url)
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	if got := parsed.Query().Get("state"); got != "state-123" {
		t.Fatalf("oauth state = %q, want the server-stored state without browser binding", got)
	}

	// This test used to assert only the query, and that gap hid a real bug: the host
	// was wrong and every parameter was right. The endpoint itself is now covered by
	// TestAuthorizeURLGoesToGoogle in endpoints_test.go.
}

func TestAuthorizeURLRequiresState(t *testing.T) {
	client := newTestClient(t, true)
	if _, err := client.AuthorizeURL("", "browser"); err == nil {
		t.Error("an empty state must be rejected; it is the CSRF guard")
	}
}

func TestAuthorizeURLRequiresCredentials(t *testing.T) {
	client := newTestClient(t, false)
	if _, err := client.AuthorizeURL("state", "browser"); err == nil {
		t.Error("an unconfigured client must refuse to build a consent URL")
	}
}

func TestMemoryTokensHonourTTL(t *testing.T) {
	cache := NewMemoryTokens()
	ctx := context.Background()

	if err := cache.Set(ctx, "dc-1", "token", time.Minute); err != nil {
		t.Fatal(err)
	}
	token, _, ok := cache.Get(ctx, "dc-1")
	if !ok || token != "token" {
		t.Errorf("Get = %q/%v, want the stored token", token, ok)
	}

	if err := cache.Delete(ctx, "dc-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := cache.Get(ctx, "dc-1"); ok {
		t.Error("a deleted token must not resolve; this is what a disconnect relies on")
	}
}

func TestMemoryTokensRejectNonPositiveTTL(t *testing.T) {
	cache := NewMemoryTokens()
	if err := cache.Set(context.Background(), "dc-1", "token", 0); err == nil {
		t.Error("a zero TTL must be rejected; an immortal cached token is a delayed revocation")
	}
}

func TestMemoryTokensAreSafeForConcurrentUse(t *testing.T) {
	cache := NewMemoryTokens()
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := string(rune('a' + n%26))
			_ = cache.Set(ctx, id, "token", time.Minute)
			_, _, _ = cache.Get(ctx, id)
		}(i)
	}
	wg.Wait()
}

func TestStale(t *testing.T) {
	now := time.Now()

	if !Stale(nil, now) {
		t.Error("a never-used connection should be reported stale; it points at a configuration problem")
	}

	fresh := now.Add(-time.Hour)
	if Stale(&fresh, now) {
		t.Error("a recently used connection must not be stale")
	}

	old := now.Add(-48 * time.Hour)
	if !Stale(&old, now) {
		t.Error("a long-unused connection should be reported stale")
	}
}

func TestDisconnectLeavesUserFilesAlone(t *testing.T) {
	resolver := &stubResolver{
		connection: Connection{ID: "dc-1", Status: StatusConnected, RefreshToken: "rt"},
	}
	client := newTestClient(t, false)
	client.conns = resolver
	// Revocation at Google is skipped when unconfigured, but the local status
	// change and token drop must still happen.
	if err := client.Disconnect(context.Background(), persistence.Scope{}); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}

	if len(resolver.markedStatus) != 1 || resolver.markedStatus[0] != StatusDisconnected {
		t.Errorf("marked statuses = %v, want a single disconnected", resolver.markedStatus)
	}
}

func TestDisconnectReportsAMissingConnection(t *testing.T) {
	client := newTestClient(t, false)
	client.conns = &stubResolver{err: persistence.ErrNoConnection}

	err := client.Disconnect(context.Background(), persistence.Scope{})
	if err == nil {
		t.Error("disconnecting with no connection must report not found")
	}
}
