package drive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/shovan04/storvia-project/internal/persistence"
	"github.com/shovan04/storvia-project/internal/storage/policy"
)

// folderMimeType is the Drive mime type for a folder.
const folderMimeType = "application/vnd.google-apps.folder"

// FolderState describes the app-managed folder on a connection.
type FolderState struct {
	// FolderID is the folder Storvia writes into.
	FolderID string
	// Exists reports whether that folder is still present in the account.
	Exists bool
	// Ambiguous is true when more than one folder carries the Storvia name.
	//
	// It is reported rather than resolved. Picking one would silently change which
	// folder existing files live in, and creating another would make the ambiguity
	// permanent. The caller asks the user what to do.
	Ambiguous bool
	// Candidates lists the folder ids found, for that conversation.
	Candidates []string
	// Name is the configured folder name.
	Name string
}

// EnsureRootFolder creates the app-managed folder, or returns the existing one.
//
// This runs once, at connection time. Everything Storvia stores goes in this folder,
// which is what makes "delete everything Storvia holds" a single, findable action for
// the user.
//
// It is deliberately *not* called on the request path. If the folder later disappears,
// the correct response is to tell the user, not to create a replacement and start
// writing somewhere they did not choose.
func (c *Client) EnsureRootFolder(ctx context.Context, scope persistence.Scope) (FolderState, error) {
	return c.ensureRootFolder(ctx, scope, true)
}

// FindRootFolder reports on the app-managed folder without creating anything.
//
// Use this when the folder may have been deleted by the user: the honest answer is that
// it is gone, and the fix is an explicit action, not a silent one.
func (c *Client) FindRootFolder(ctx context.Context, scope persistence.Scope) (FolderState, error) {
	return c.ensureRootFolder(ctx, scope, false)
}

func (c *Client) ensureRootFolder(ctx context.Context, scope persistence.Scope, allowCreate bool) (FolderState, error) {
	connection, err := c.usableConnectionForFolder(ctx, scope)
	if err != nil {
		return FolderState{}, err
	}

	name := c.cfg.RootFolderName
	if name == "" {
		name = "Storvia"
	}
	state := FolderState{Name: name}

	found, err := c.foldersNamed(ctx, connection, name)
	if err != nil {
		return FolderState{}, err
	}

	switch {
	case len(found) == 1:
		state.FolderID, state.Exists = found[0], true
		return state, nil

	case len(found) > 1:
		// Never guess. Two folders called Storvia means the user has a copy and an
		// original, or a duplicate from an earlier bug; either way, choosing one
		// silently would split their files across two places.
		state.Ambiguous = true
		state.Candidates = found
		sort.Strings(state.Candidates)
		return state, nil
	}

	if !allowCreate {
		return state, nil
	}

	created, err := c.createFolder(ctx, connection, name)
	if err != nil {
		return FolderState{}, err
	}
	state.FolderID, state.Exists = created, true
	return state, nil
}

// foldersNamed lists non-trashed folders with the given name.
//
// Trashed folders are excluded on purpose. If the user deletes the Storvia folder and
// re-runs recovery, reusing the trashed one would write into their trash.
func (c *Client) foldersNamed(ctx context.Context, connection Connection, name string) ([]string, error) {
	token, err := c.accessToken(ctx, connection)
	if err != nil {
		return nil, err
	}

	query := url.Values{}
	query.Set("q", fmt.Sprintf("name = %q and mimeType = %q and trashed = false",
		name, folderMimeType))
	query.Set("fields", "files(id,name,mimeType,trashed)")
	query.Set("supportsAllDrives", "true")
	query.Set("pageSize", "100")
	// Spaces rather than "me": the app acts on a user's Drive, and "me" is an alias
	// for the authorised user, which is the same thing here but less explicit.
	query.Set("spaces", "drive")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		driveFilesURL+"?"+query.Encode(), nil)
	if err != nil {
		return nil, &policy.ProviderError{
			Provider: c.Name(), Operation: "FindFolder", Class: policy.ClassValidation, Err: err,
		}
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, c.normalizeTransportError("FindFolder", err)
	}
	defer drainAndClose(resp)

	if resp.StatusCode >= 400 {
		return nil, c.normalizeStatus("FindFolder", resp)
	}

	var payload struct {
		Files []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, &policy.ProviderError{
			Provider: c.Name(), Operation: "FindFolder", Class: policy.ClassUnknown,
			SafeDetail: "drive returned a folder listing storvia could not read", Err: err,
		}
	}

	ids := make([]string, 0, len(payload.Files))
	for _, file := range payload.Files {
		if strings.EqualFold(file.Name, name) {
			ids = append(ids, file.ID)
		}
	}
	return ids, nil
}

// createFolder makes a folder in the account's My Drive root.
func (c *Client) createFolder(ctx context.Context, connection Connection, name string) (string, error) {
	token, err := c.accessToken(ctx, connection)
	if err != nil {
		return "", err
	}

	body, err := json.Marshal(map[string]any{
		"name":     name,
		"mimeType": folderMimeType,
	})
	if err != nil {
		return "", &policy.ProviderError{
			Provider: c.Name(), Operation: "CreateFolder", Class: policy.ClassValidation, Err: err,
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, driveFilesURL, bytes.NewReader(body))
	if err != nil {
		return "", &policy.ProviderError{
			Provider: c.Name(), Operation: "CreateFolder", Class: policy.ClassValidation, Err: err,
		}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", c.normalizeTransportError("CreateFolder", err)
	}
	defer drainAndClose(resp)

	if resp.StatusCode >= 400 {
		return "", c.normalizeStatus("CreateFolder", resp)
	}

	var payload struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil || payload.ID == "" {
		return "", &policy.ProviderError{
			Provider: c.Name(), Operation: "CreateFolder", Class: policy.ClassUnknown,
			SafeDetail: "drive accepted the folder but returned no id",
		}
	}
	return payload.ID, nil
}

// usableConnectionForFolder resolves the connection for folder work.
//
// Unlike the file-transfer path this tolerates a missing RootFolderID, because finding
// or creating that folder is precisely how the ID gets set in the first place.
func (c *Client) usableConnectionForFolder(ctx context.Context, scope persistence.Scope) (Connection, error) {
	connection, err := c.conns.Connection(ctx, scope)
	if err != nil {
		return Connection{}, &policy.ProviderError{
			Provider: c.Name(), Operation: "Folder", Class: policy.ClassConsentRequired,
			SafeDetail: "no google drive connection exists for this account", Err: err,
		}
	}
	if connection.Status != StatusConnected {
		return Connection{}, &policy.ProviderError{
			Provider: c.Name(), Operation: "Folder", Class: policy.ClassConsentRequired,
			SafeDetail: "the google drive connection is not usable; reconnect it",
		}
	}
	return connection, nil
}
