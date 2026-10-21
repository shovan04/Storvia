package drive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/shovan04/storvia-project/internal/storage/policy"
)

// changesEndpoint is Drive's change log. It is separate from driveFilesURL for the same
// reason driveAboutURL is: it is its own route, not a sub-path of /files.
const changesEndpoint = "https://www.googleapis.com/drive/v3/changes"

// maxChangePages bounds one polling pass.
//
// The change log is effectively unbounded for a busy account, and a poller that tries
// to drain it in one pass would hold its cursor and never notice a revoked grant. The
// next pass continues where this one stopped.
const maxChangePages = 5

// Change is one entry from Drive's change log.
type Change struct {
	// FileID is the file that changed.
	FileID string
	// Removed is true when the change is a deletion.
	Removed bool
}

// ChangePage is one page of the change log plus the cursor for the next one.
type ChangePage struct {
	Changes []Change
	// NextCursor is the page token to resume from. It is empty when the log is
	// exhausted, which means the poller has caught up.
	NextCursor string
	// NewestCursor is the newest change token seen, which is where a restart should
	// resume from if the cursor was never stored.
	NewestCursor string
}

// ListChanges reads Drive's change log from a cursor.
//
// The cursor is the only thing that makes this incremental. Advancing it before the
// pages are processed would skip changes; advancing it after means a crash re-reads a
// page rather than losing it. Re-reading is the safe direction: every change here is
// idempotent.
func (c *Client) ListChanges(ctx context.Context, connectionID, cursor string, limit int) (ChangePage, error) {
	connection, err := c.conns.ConnectionByID(ctx, connectionID)
	if err != nil {
		return ChangePage{}, &policy.ProviderError{
			Provider: c.Name(), Operation: "ListChanges", Class: policy.ClassConsentRequired,
			SafeDetail: "the drive connection could not be read", Err: err,
		}
	}
	token, err := c.accessToken(ctx, connection)
	if err != nil {
		return ChangePage{}, err
	}

	if limit <= 0 || limit > 1000 {
		limit = 500
	}

	query := url.Values{}
	query.Set("pageSize", fmt.Sprint(limit))
	query.Set("fields", "nextPageToken,newStartPageToken,changes(fileId,removed)")
	query.Set("supportsAllDrives", "true")
	// Drive requires pageToken on every call, including the first. Without a stored
	// cursor there is nothing to send, so one is minted from the start-page endpoint.
	if cursor == "" {
		started, err := c.StartPageToken(ctx, connection)
		if err != nil {
			return ChangePage{}, err
		}
		cursor = started
	}
	query.Set("pageToken", cursor)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, changesEndpoint+"?"+query.Encode(), nil)
	if err != nil {
		return ChangePage{}, &policy.ProviderError{
			Provider: c.Name(), Operation: "ListChanges", Class: policy.ClassValidation, Err: err,
		}
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return ChangePage{}, c.normalizeTransportError("ListChanges", err)
	}
	defer drainAndClose(resp)

	if resp.StatusCode == http.StatusBadRequest && isExpiredPageToken(resp) {
		// isExpiredPageToken restores the body it read; anything downstream that
		// classifies this error would otherwise see an empty response.
		// Drive returns 400 for a page token that has expired. That is recoverable:
		// start again from the newest token and accept that some changes in the gap
		// are not seen by this pass.
		return ChangePage{}, &policy.ProviderError{
			Provider: c.Name(), Operation: "ListChanges", Class: policy.ClassConfiguration,
			SafeDetail: "the stored changes cursor has expired; reset it to continue polling",
		}
	}
	if resp.StatusCode >= 400 {
		return ChangePage{}, c.normalizeStatus("ListChanges", resp)
	}

	var payload struct {
		NextPageToken     string `json:"nextPageToken"`
		NewStartPageToken string `json:"newStartPageToken"`
		Changes           []struct {
			FileID  string `json:"fileId"`
			Removed bool   `json:"removed"`
		} `json:"changes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return ChangePage{}, &policy.ProviderError{
			Provider: c.Name(), Operation: "ListChanges", Class: policy.ClassUnknown,
			SafeDetail: "drive returned a change page storvia could not read", Err: err,
		}
	}

	page := ChangePage{NextCursor: payload.NextPageToken, NewestCursor: payload.NewStartPageToken}
	for _, change := range payload.Changes {
		if change.FileID == "" {
			continue
		}
		page.Changes = append(page.Changes, Change{FileID: change.FileID, Removed: change.Removed})
	}
	return page, nil
}

// isExpiredPageToken reports whether a 400 was caused by a stale page token.
//
// Google does not use a dedicated code for this, so the message is matched. Guessing
// wrong here means either a silent reset on an unrelated 400, or never recovering from
// an expired cursor, so the check is deliberately narrow.
func isExpiredPageToken(resp *http.Response) bool {
	body, _ := readBounded(resp.Body)
	// The body is put back. Reading it here and then letting normalizeStatus read it
	// again is how a real Google error message gets replaced with nothing.
	resp.Body = io.NopCloser(bytes.NewReader(body))

	// Google words this several ways ("Invalid Value", "Required parameter:
	// pageToken", "the page token has expired") and the spacing is not consistent
	// between them, so the text is normalised before matching rather than guessed at.
	text := strings.ToLower(strings.ReplaceAll(string(body), " ", ""))
	return strings.Contains(text, "pagetoken") &&
		(strings.Contains(text, "expired") || strings.Contains(text, "invalid") ||
			strings.Contains(text, "cannotbeused") || strings.Contains(text, "required"))
}

// StartPageToken returns a token marking "now" in Drive's change log.
//
// It is how a first poll begins. Passing it to changes.list means the poller starts
// from this moment and sees changes made after it, which is the right default: the
// alternative is walking the entire history of an account that may have been using
// Drive for years.
func (c *Client) StartPageToken(ctx context.Context, connection Connection) (string, error) {
	token, err := c.accessToken(ctx, connection)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, changesEndpoint+"/startPageToken", nil)
	if err != nil {
		return "", &policy.ProviderError{
			Provider: c.Name(), Operation: "StartPageToken", Class: policy.ClassValidation, Err: err,
		}
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", c.normalizeTransportError("StartPageToken", err)
	}
	defer drainAndClose(resp)

	if resp.StatusCode >= 400 {
		return "", c.normalizeStatus("StartPageToken", resp)
	}

	var payload struct {
		StartPageToken string `json:"startPageToken"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil || payload.StartPageToken == "" {
		return "", &policy.ProviderError{
			Provider: c.Name(), Operation: "StartPageToken", Class: policy.ClassUnknown,
			SafeDetail: "drive returned no start page token",
		}
	}
	return payload.StartPageToken, nil
}

// ChangeCursor reads the stored cursor for a connection.
func (c *Client) ChangeCursor(ctx context.Context, connectionID string) (string, error) {
	connection, err := c.conns.ConnectionByID(ctx, connectionID)
	if err != nil {
		return "", err
	}
	if connection.ChangesCursor == nil {
		return "", nil
	}
	return *connection.ChangesCursor, nil
}
