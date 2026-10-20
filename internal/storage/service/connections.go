package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"strings"

	"github.com/shovan04/storvia/internal/apperr"
	"github.com/shovan04/storvia/internal/persistence"
	"github.com/shovan04/storvia/internal/storage/provider/drive"
)

// OAuthStateTTL is how long a pending authorization stays valid.
const OAuthStateTTL = 10 * time.Minute

// Connector is the Drive consent surface the connection service drives.
//
// Declaring it here, in the consumer, keeps the service free of OAuth types and
// makes it testable without a Google endpoint.
type Connector interface {
	// BeginConnect records a pending authorization bound to browserBinding and
	// returns the consent URL.
	BeginConnect(ctx context.Context, scope persistence.Scope, browserBinding string) (authorizeURL string, err error)
	// CompleteConnect exchanges the code for tokens and stores the connection.
	CompleteConnect(ctx context.Context, scope persistence.Scope, code, state, browserBinding string) error
	// Status reports the connection state without secrets.
	Status(ctx context.Context, scope persistence.Scope) (ConnectionStatus, error)
	// Disconnect stops new Drive requests and drops the stored grant. It leaves
	// the user's Drive files untouched.
	Disconnect(ctx context.Context, scope persistence.Scope) error
}

// RedirectConnector is the optional extension used by the development admin
// panel. The normal API flow keeps using Connector's configured callback.
type RedirectConnector interface {
	BeginConnectAt(ctx context.Context, scope persistence.Scope, browserBinding, redirectURI string) (authorizeURL string, err error)
	CompleteConnectAt(ctx context.Context, scope persistence.Scope, code, state, browserBinding, redirectURI string) error
}

// ConnectionStatus is the client-visible connection state.
type ConnectionStatus struct {
	Connected     bool
	Status        string
	Subject       string
	GrantedScopes []string
	FolderPresent bool
	LastSuccessAt *time.Time
	Stale         bool
	// UsesAppDataFolder reports whether files are stored in Drive's hidden
	// appDataFolder rather than a visible folder in My Drive.
	UsesAppDataFolder bool
	// Email, DisplayName and PictureURL identify the connected Google account so an
	// operator can see who they are acting as.
	//
	// They are display only. ConnectionStatus is never an authorisation input, and the
	// subject remains the identity.
	Email       string
	DisplayName string
	PictureURL  string
}

// ConnectionService handles the Drive connection lifecycle.
type ConnectionService struct {
	connector Connector
}

// NewConnectionService builds the service.
func NewConnectionService(connector Connector) *ConnectionService {
	return &ConnectionService{connector: connector}
}

// Status reports the Drive connection state.
func (s *ConnectionService) Status(ctx context.Context, scope persistence.Scope) (ConnectionStatus, error) {
	return s.connector.Status(ctx, scope)
}

// Disconnect removes the connection.
//
// It stops new Drive requests and new uploads, and leaves the user's own Drive
// files in place. Storvia never deletes user data as a side effect of a
// connection change.
func (s *ConnectionService) Disconnect(ctx context.Context, scope persistence.Scope) error {
	return s.connector.Disconnect(ctx, scope)
}

// UsesAppDataFolder reports whether files are stored in Drive's hidden appDataFolder
// rather than a visible folder in My Drive.
func (s *ConnectionService) UsesAppDataFolder() bool {
	connector, ok := s.connector.(*DriveConnector)
	if !ok {
		return false
	}
	return connector.client.UsesAppDataFolder()
}

// BeginConnect starts the consent flow and returns the URL the browser must open.
func (s *ConnectionService) BeginConnect(ctx context.Context, scope persistence.Scope, browserBinding string) (string, error) {
	if browserBinding == "" {
		// An unbound flow would accept a callback from any browser, which is the
		// OAuth account-mix-up attack.
		return "", apperr.Authentication("a signed-in session is required to connect google drive")
	}
	url, err := s.connector.BeginConnect(ctx, scope, browserBinding)
	if err != nil {
		return "", apperr.Wrap(err, apperr.KindInternal, "Internal error",
			"the google authorization could not be started")
	}
	return url, nil
}

// BeginConnectAt starts a flow at an explicitly configured callback. It is used
// only by the development panel, never with a browser-supplied redirect URI.
func (s *ConnectionService) BeginConnectAt(ctx context.Context, scope persistence.Scope, browserBinding, redirectURI string) (string, error) {
	if browserBinding == "" || redirectURI == "" {
		return "", apperr.Authentication("a bound browser session and redirect URI are required")
	}
	connector, ok := s.connector.(RedirectConnector)
	if !ok {
		return "", apperr.ProviderNotConfigured("the configured drive connector does not support the admin callback")
	}
	url, err := connector.BeginConnectAt(ctx, scope, browserBinding, redirectURI)
	if err != nil {
		return "", apperr.Wrap(err, apperr.KindInternal, "Internal error", "the google authorization could not be started")
	}
	return url, nil
}

// CompleteConnect finishes the consent flow.
func (s *ConnectionService) CompleteConnect(ctx context.Context, scope persistence.Scope, code, state, browserBinding string) error {
	return s.connector.CompleteConnect(ctx, scope, code, state, browserBinding)
}

// CompleteConnectAt finishes a flow at an explicitly configured callback.
func (s *ConnectionService) CompleteConnectAt(ctx context.Context, scope persistence.Scope, code, state, browserBinding, redirectURI string) error {
	connector, ok := s.connector.(RedirectConnector)
	if !ok {
		return apperr.ProviderNotConfigured("the configured drive connector does not support the admin callback")
	}
	return connector.CompleteConnectAt(ctx, scope, code, state, browserBinding, redirectURI)
}

// DriveConnector implements Connector over the Drive client.
type DriveConnector struct {
	client *drive.Client
}

// NewDriveConnector builds a connector.
func NewDriveConnector(client *drive.Client) *DriveConnector {
	return &DriveConnector{client: client}
}

// BeginConnect starts the consent flow.
func (d *DriveConnector) BeginConnect(ctx context.Context, scope persistence.Scope, browserBinding string) (string, error) {
	return d.BeginConnectAt(ctx, scope, browserBinding, d.client.RedirectURI())
}

// BeginConnectAt starts a flow with a registered redirect URI.
func (d *DriveConnector) BeginConnectAt(ctx context.Context, scope persistence.Scope, browserBinding, redirectURI string) (string, error) {
	state, err := d.client.NewState()
	if err != nil {
		return "", apperr.Wrap(err, apperr.KindInternal, "Internal error", "could not start the authorization")
	}
	d.client.RememberState(state, browserBinding)
	return d.client.AuthorizeURLWithRedirect(state, browserBinding, redirectURI)
}

// CompleteConnect finishes the consent flow.
func (d *DriveConnector) CompleteConnect(ctx context.Context, scope persistence.Scope, code, state, browserBinding string) error {
	return d.completeConnectAt(ctx, scope, code, state, browserBinding, d.client.RedirectURI())
}

// CompleteConnectAt finishes a flow with a registered redirect URI.
func (d *DriveConnector) CompleteConnectAt(ctx context.Context, scope persistence.Scope, code, state, browserBinding, redirectURI string) error {
	return d.completeConnectAt(ctx, scope, code, state, browserBinding, redirectURI)
}

func (d *DriveConnector) completeConnectAt(ctx context.Context, scope persistence.Scope, code, state, browserBinding, redirectURI string) error {
	result, err := d.client.ExchangeWithRedirect(ctx, drive.Scope{TenantID: scope.TenantID, UserID: scope.UserID}, code, state, browserBinding, redirectURI)
	if err == nil {
		// The app folder is created here, at connection time, rather than lazily on
		// the first upload. Doing it later would mean the first upload could fail for
		// a reason the user cannot act on, and it would happen at the worst moment.
		//
		// A failure here is logged rather than fatal: the account is connected and the
		// grant is stored, so failing the whole callback would discard a working
		// authorization and force the user to consent again.
		if folderErr := d.ensureRootFolder(ctx, scope, result.Connection.ID); folderErr != nil {
			d.client.LogWarn(ctx, "connected, but the storvia drive folder could not be prepared",
				"reason", folderErr.Error())
		}
		return nil
	}
	if errors.Is(err, drive.ErrNotConfigured) {
		return apperr.ProviderNotConfigured("google drive is not configured on this deployment")
	}
	if apperr.Is(err, apperr.KindConsentRequired) {
		return err
	}
	return apperr.Wrap(err, apperr.KindInternal, "Internal error", "the drive connection could not be completed")
}

// ensureRootFolder finds or creates the app-managed folder and records its id.
func (d *DriveConnector) ensureRootFolder(ctx context.Context, scope persistence.Scope, connectionID string) error {
	state, err := d.client.EnsureRootFolder(ctx, scope)
	if err != nil {
		return err
	}
	if state.Ambiguous {
		// Reported, not resolved. Picking one of two folders called Storvia would
		// silently split the user's files; making another would make it permanent.
		return fmt.Errorf("drive: %d folders are named %q (%s); the storvia drive folder is ambiguous and was not chosen",
			len(state.Candidates), state.Name, strings.Join(state.Candidates, ", "))
	}
	if state.FolderID == "" {
		return fmt.Errorf("drive: the %q folder was not created", state.Name)
	}
	return d.client.SetRootFolderID(ctx, connectionID, state.FolderID)
}

// RecoverRootFolder re-runs folder discovery on an explicit user request.
//
// It is the recovery path when the folder has been deleted. It is not automatic,
// because creating a replacement without being asked would move a boundary the user
// controls without telling them.
//
// It will reuse an existing folder if one is found; only an explicit choice resolves an
// ambiguous name.
func (d *DriveConnector) RecoverRootFolder(ctx context.Context, scope persistence.Scope, connectionID string) error {
	return d.ensureRootFolder(ctx, scope, connectionID)
}

// EnsureRootFolder creates the app-managed folder if it does not exist and records
// its id. It is safe to call at any time; an existing folder is reused.
func (s *ConnectionService) EnsureRootFolder(ctx context.Context, scope persistence.Scope) error {
	connector, ok := s.connector.(FolderConnector)
	if !ok {
		return apperr.ProviderNotConfigured("the configured drive connector cannot manage the storvia folder")
	}
	connection, err := connector.ConnectionIDFor(ctx, scope)
	if err != nil {
		return err
	}
	return connector.RecoverRootFolder(ctx, scope, connection)
}

// FolderConnector is the folder-management subset of the Drive connector.
type FolderConnector interface {
	ConnectionIDFor(ctx context.Context, scope persistence.Scope) (string, error)
	RecoverRootFolder(ctx context.Context, scope persistence.Scope, connectionID string) error
}

// ConnectionIDFor returns the tenant's Drive connection id.
func (d *DriveConnector) ConnectionIDFor(ctx context.Context, scope persistence.Scope) (string, error) {
	id, err := d.client.ConnectionID(ctx, scope)
	if err != nil {
		return "", apperr.Wrap(err, apperr.KindInternal, "Internal error", "the drive connection could not be resolved")
	}
	if id == "" {
		return "", apperr.NotFound("no google drive connection exists for this account")
	}
	return id, nil
}

// Status reports the connection state.
func (d *DriveConnector) Status(ctx context.Context, scope persistence.Scope) (ConnectionStatus, error) {
	status := ConnectionStatus{Status: "disconnected", GrantedScopes: []string{drive.ScopeDriveFile}}

	raw, err := d.client.ConnectionStatus(ctx, scope)
	if err != nil {
		if errors.Is(err, persistence.ErrNoConnection) {
			return status, nil
		}
		return ConnectionStatus{}, apperr.Wrap(err, apperr.KindInternal, "Internal error",
			"the drive connection state could not be read")
	}

	status.Status = raw.Status
	status.Subject = raw.Subject
	// A missing profile is not a failure: a connection made before the profile columns
	// existed has none, and refusing to report status because of a cosmetic field would
	// be the wrong trade.
	if profile, err := d.client.AccountProfile(ctx, scope); err == nil {
		status.Email = profile.Email
		status.DisplayName = profile.DisplayName
		status.PictureURL = profile.PictureURL
	}
	// In app-data mode there is no visible folder, but appDataFolder is always
	// available, so the "folder present" flag is true.
	if d.client.UsesAppDataFolder() {
		status.FolderPresent = true
		status.UsesAppDataFolder = true
	} else {
		status.FolderPresent = raw.RootFolderID != ""
	}
	status.LastSuccessAt = raw.LastSuccessAt
	status.Stale = drive.Stale(raw.LastSuccessAt, time.Now())
	status.Connected = raw.Status == drive.StatusConnected
	if !d.client.Configured() {
		// Report the deployment gap rather than implying the user's account is at
		// fault.
		status.Status = "provider_not_configured"
	}
	return status, nil
}

// Disconnect removes the connection.
func (d *DriveConnector) Disconnect(ctx context.Context, scope persistence.Scope) error {
	if err := d.client.Disconnect(ctx, scope); err != nil {
		if errors.Is(err, persistence.ErrNoConnection) {
			return apperr.NotFound("no google drive connection exists for this account")
		}
		return apperr.Wrap(err, apperr.KindInternal, "Internal error", "the drive connection could not be removed")
	}
	return nil
}

// ensure the fmt import stays used if the file evolves; the reference below keeps
// the import honest today.
var _ = fmt.Sprintf
