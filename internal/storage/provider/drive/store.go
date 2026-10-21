package drive

import (
	"context"
	"errors"
	"time"

	"github.com/shovan04/storvia-project/internal/persistence"
)

// Store is the persistence surface the Drive provider needs.
//
// It is declared here, in the consumer, rather than importing a repository
// package directly. That inversion is what keeps the provider testable with a
// fake and stops a storage change from rippling into OAuth code.
type Store interface {
	// Resolve returns the connection for a scope with its token decrypted. It
	// returns ErrNoConnection when the tenant has not connected Drive.
	Resolve(ctx context.Context, scope persistence.Scope) (id, subject, refreshToken, status, rootFolderID string, err error)
	// Upsert stores or refreshes a connection, and the profile of the account behind
	// it, and returns its ID.
	Upsert(ctx context.Context, scope persistence.Scope, subject, refreshToken string,
		profile persistence.ConnectionProfile) (string, error)
	// UpdateRefreshToken replaces a rotated refresh token.
	UpdateRefreshToken(ctx context.Context, connectionID, refreshToken string) error
	// SetStatus changes a connection's status.
	SetStatus(ctx context.Context, connectionID, status string) error
	// SetRootFolderID records the app-managed folder.
	SetRootFolderID(ctx context.Context, connectionID, folderID string) error
	// AccountProfile reports who the connected Google account is, for display.
	//
	// It is separate from Connection because Connection decrypts a refresh token and
	// rendering a name has no business holding one.
	AccountProfile(ctx context.Context, scope persistence.Scope) (persistence.ConnectionProfile, error)
	// SaveAccountProfile persists the display columns only, never a credential.
	SaveAccountProfile(ctx context.Context, scope persistence.Scope, profile persistence.ConnectionProfile) error
	// ConnectionByID reads a connection for background work.
	ConnectionByID(ctx context.Context, connectionID string) (persistence.DriveConnection, error)
	// SetChangesCursor advances how far the change log has been processed.
	SetChangesCursor(ctx context.Context, connectionID, cursor string) error
	// ConnectedConnectionIDs lists connections worth polling.
	ConnectedConnectionIDs(ctx context.Context) ([]string, error)
}

// deref reads a nullable string column as a plain string.
func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// storeResolver adapts a Store to the ConnectionResolver interface the client
// consumes. The indirection exists so the client keeps working against the small
// Connection struct it needs, not the storage record shape.
type storeResolver struct{ store Store }

// NewResolver builds a ConnectionResolver backed by a Store.
func NewResolver(store Store) ConnectionResolver { return &storeResolver{store: store} }

// Connection returns the connection for a scope.
//
// A tenant with no connection returns ErrNoConnection rather than a zero
// Connection, so the caller can distinguish "not connected" from "connected but
// broken".
func (r *storeResolver) Connection(ctx context.Context, scope persistence.Scope) (Connection, error) {
	id, subject, token, status, folderID, err := r.store.Resolve(ctx, scope)
	if err != nil {
		if errors.Is(err, persistence.ErrNoConnection) {
			return Connection{}, persistence.ErrNoConnection
		}
		return Connection{}, err
	}
	return Connection{
		ID:           id,
		TenantID:     scope.TenantID,
		UserID:       scope.UserID,
		Subject:      subject,
		RefreshToken: token,
		RootFolderID: folderID,
		Status:       status,
	}, nil
}

// AccountProfile reports the connected account for display.
func (r *storeResolver) AccountProfile(ctx context.Context, scope persistence.Scope) (persistence.ConnectionProfile, error) {
	return r.store.AccountProfile(ctx, scope)
}

// SaveAccountProfile persists the display columns only.
func (r *storeResolver) SaveAccountProfile(ctx context.Context, scope persistence.Scope, profile persistence.ConnectionProfile) error {
	return r.store.SaveAccountProfile(ctx, scope, profile)
}

// SaveConnection stores or refreshes a connection.
func (r *storeResolver) SaveConnection(ctx context.Context, connection Connection) error {
	id, err := r.store.Upsert(ctx, persistence.Scope{
		TenantID: connection.TenantID,
		UserID:   connection.UserID,
	}, connection.Subject, connection.RefreshToken, persistence.ConnectionProfile{
		Email:       connection.Email,
		DisplayName: connection.DisplayName,
		PictureURL:  connection.PictureURL,
	})
	if err != nil {
		return err
	}
	connection.ID = id
	if connection.RootFolderID != "" {
		if err := r.store.SetRootFolderID(ctx, id, connection.RootFolderID); err != nil {
			return err
		}
	}
	return nil
}

// MarkStatus changes a connection's status.
//
// A status change whose connection has vanished is treated as success: the
// intended end state, disconnected or needing reauthorization, already holds.
func (r *storeResolver) MarkStatus(ctx context.Context, connectionID, status string) error {
	return r.store.SetStatus(ctx, connectionID, status)
}

// SetRootFolderID records the app-managed folder on a connection.
//
// It is exposed on the client so the service layer can record the folder it just
// created without reaching for the repository directly.
func (c *Client) SetRootFolderID(ctx context.Context, connectionID, folderID string) error {
	store, ok := c.conns.(*storeResolver)
	if !ok {
		return errors.New("drive: this connection store cannot persist the root folder")
	}
	return store.store.SetRootFolderID(ctx, connectionID, folderID)
}

// LogWarn records a non-fatal problem during connection setup.
func (c *Client) LogWarn(ctx context.Context, message string, args ...any) {
	if c.log == nil {
		return
	}
	c.log.WarnContext(ctx, message, args...)
}

// ConnectionByID reads a connection by id for background work.
func (r *storeResolver) ConnectionByID(ctx context.Context, connectionID string) (Connection, error) {
	stored, err := r.store.ConnectionByID(ctx, connectionID)
	if err != nil {
		if errors.Is(err, persistence.ErrNoConnection) {
			return Connection{}, persistence.ErrNoConnection
		}
		return Connection{}, err
	}
	return Connection{
		ID:            stored.ID,
		TenantID:      stored.TenantID,
		UserID:        stored.UserID,
		Subject:       stored.Subject,
		RefreshToken:  stored.PlaintextRefreshToken,
		RootFolderID:  deref(stored.RootFolderID),
		Status:        stored.Status,
		ChangesCursor: stored.ChangesCursor,
	}, nil
}

// SetChangesCursor advances the change-log cursor.
func (r *storeResolver) SetChangesCursor(ctx context.Context, connectionID, cursor string) error {
	return r.store.SetChangesCursor(ctx, connectionID, cursor)
}

// SetRefreshToken persists a rotated refresh token.
func (c *Client) SetRefreshToken(ctx context.Context, connectionID, refreshToken string) error {
	store, ok := c.conns.(*storeResolver)
	if !ok {
		return errors.New("drive: this connection store cannot persist rotated tokens")
	}
	return store.store.UpdateRefreshToken(ctx, connectionID, refreshToken)
}

// StalenessThreshold is how long without a successful call before a connection is
// reported as possibly stale in the status endpoint.
const StalenessThreshold = 24 * time.Hour

// Stale reports whether lastSuccess is old enough to be worth surfacing.
func Stale(lastSuccess *time.Time, now time.Time) bool {
	if lastSuccess == nil {
		// Never used is different from used long ago: it points at a
		// configuration or connection problem rather than a stale session.
		return true
	}
	return now.Sub(*lastSuccess) > StalenessThreshold
}

// AdvanceChangesCursor records how far the change log has been read.
func (c *Client) AdvanceChangesCursor(ctx context.Context, connectionID, cursor string) error {
	store, ok := c.conns.(*storeResolver)
	if !ok {
		return errors.New("drive: this connection store cannot persist a changes cursor")
	}
	return store.store.SetChangesCursor(ctx, connectionID, cursor)
}

// ConnectedIDs lists the connections whose change logs should be polled.
func (c *Client) ConnectedIDs(ctx context.Context) ([]string, error) {
	store, ok := c.conns.(*storeResolver)
	if !ok {
		return nil, errors.New("drive: this connection store cannot be enumerated")
	}
	return store.store.ConnectedConnectionIDs(ctx)
}
