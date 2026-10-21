package drive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/shovan04/storvia-project/internal/persistence"
	"github.com/shovan04/storvia-project/internal/storage/policy"
)

// The provider's endpoints are absolute constants pointing at googleapis.com, so a
// httptest.Server alone is not enough: without interception these tests would talk to
// the real Google. A RoundTripper keeps the production code free of a test seam and
// makes it impossible for this suite to reach the network by accident.

// newFileTestClient builds a client whose provider calls never leave the process.
func newFileTestClient(t *testing.T, handler http.Handler) (*Client, *stubResolver) {
	t.Helper()

	resolver := &stubResolver{connection: Connection{
		ID:           "dc-1",
		TenantID:     "tenant-a",
		UserID:       "user-1",
		Status:       StatusConnected,
		RootFolderID: "folder-1",
		RefreshToken: "refresh-token",
	}}

	client, err := New(context.Background(), Config{
		Credentials: Credentials{
			ClientID:     "client-id.apps.googleusercontent.com",
			ClientSecret: "client-secret",
			RedirectURI:  "http://127.0.0.1:8080/cb",
		},
		Scopes: []string{ScopeDriveFile},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if strings.Contains(req.URL.Path, "/token") {
				return jsonResponse(http.StatusOK,
					`{"access_token":"access-token","expires_in":3600,"token_type":"Bearer"}`), nil
			}
			return serveHandler(handler, req)
		})},
	}, ClientDeps{
		Tokens:   NewMemoryTokens(),
		Resolver: resolver,
		States:   NewStateStore(time.Minute),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client, resolver
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// serveHandler runs a handler in memory and captures its response.
func serveHandler(h http.Handler, req *http.Request) (*http.Response, error) {
	rec := &responseRecorder{header: http.Header{}, body: &bytes.Buffer{}}
	h.ServeHTTP(rec, req)
	return rec.response(), nil
}

type responseRecorder struct {
	header http.Header
	body   *bytes.Buffer
	status int
}

func (r *responseRecorder) Header() http.Header { return r.header }
func (r *responseRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(b)
}
func (r *responseRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}
func (r *responseRecorder) response() *http.Response {
	status := r.status
	if status == 0 {
		status = http.StatusOK
	}
	resp := &http.Response{
		StatusCode: status,
		Header:     r.header,
		Body:       io.NopCloser(bytes.NewReader(r.body.Bytes())),
	}
	// A real server sets this, and the download path reads it.
	resp.ContentLength = int64(r.body.Len())
	if r.header.Get("Content-Length") == "" {
		resp.ContentLength = int64(r.body.Len())
	}
	return resp
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func testScope() persistence.Scope {
	return persistence.Scope{TenantID: "tenant-a", UserID: "user-1"}
}

// TestInitiateUploadCreatesAResumableSession is the request that will be made to
// Google. The parts that matter: the resumable flag, the declared size headers, and
// the parent folder being the app-managed one rather than anything a caller chose.
func TestInitiateUploadCreatesAResumableSession(t *testing.T) {
	var (
		gotMethod, gotPath, gotQuery, gotAuth string
		gotSize, gotType, gotUploadLen        string
		gotBody                               map[string]any
	)

	client, _ := newFileTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		gotSize = r.Header.Get("X-Upload-Content-Length")
		gotType = r.Header.Get("X-Upload-Content-Type")
		gotUploadLen = r.Header.Get("X-Upload-Content-Length")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Location", "https://www.googleapis.com/upload/drive/v3/files?upload_id=SESSION")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))

	// The token cache must have a token or the call stops before the request.
	client.tokens.Set(context.Background(), "dc-1", "access-token", time.Hour)

	result, err := client.InitiateUpload(context.Background(), testScope(), InitiateUploadRequest{
		FileName: "notes.txt", MimeType: "text/plain", SizeBytes: 2048,
	})
	if err != nil {
		t.Fatalf("InitiateUpload: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	// The path must be /upload/drive/v3/files exactly. It was once this plus an
	// extra "/files", which Google answers with a 404.
	if gotPath != "/upload/drive/v3/files" {
		t.Errorf("path = %q, want /upload/drive/v3/files", gotPath)
	}
	if !strings.Contains(gotQuery, "uploadType=resumable") {
		t.Errorf("query = %q, want uploadType=resumable", gotQuery)
	}
	if gotSize != "2048" || gotUploadLen != "2048" {
		t.Errorf("declared size headers = %q/%q, want 2048", gotSize, gotUploadLen)
	}
	if gotType != "text/plain" {
		t.Errorf("content type = %q, want text/plain", gotType)
	}
	if gotAuth != "Bearer access-token" {
		t.Errorf("authorization = %q", gotAuth)
	}

	// The parent must be the app folder. A client cannot steer the write.
	parents, _ := gotBody["parents"].([]any)
	if len(parents) != 1 || parents[0] != "folder-1" {
		t.Errorf("parents = %v, want exactly [folder-1]", parents)
	}
	if gotBody["name"] != "notes.txt" {
		t.Errorf("name = %v, want notes.txt", gotBody["name"])
	}

	if result.SessionURL != "https://www.googleapis.com/upload/drive/v3/files?upload_id=SESSION" {
		t.Errorf("session URL = %q", result.SessionURL)
	}
	// A file smaller than the granularity is one chunk of its own size, which Drive
	// accepts because only the *non-final* chunks must be multiples.
	if result.ChunkSize != 2048 && !ChunkSizeIsValid(result.ChunkSize) {
		t.Errorf("advertised chunk size %d is neither the file size nor a multiple of %d",
			result.ChunkSize, DriveResumableChunkMultiple)
	}
	if result.ExpiresAt.IsZero() {
		t.Error("the session must advertise an expiry")
	}
}

// TestInitiateUploadRequiresAFileName: an unnamed file is untraceable, and Storvia
// tracks Drive files by name inside its folder.
func TestInitiateUploadRequiresAFileName(t *testing.T) {
	client, _ := newFileTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should be made")
	}))
	client.tokens.Set(context.Background(), "dc-1", "access-token", time.Hour)

	_, err := client.InitiateUpload(context.Background(), testScope(), InitiateUploadRequest{SizeBytes: 10})
	assertProviderClass(t, err, policy.ClassValidation)
}

// TestInitiateUploadRefusesAnotherTenantsConnection guards tenant isolation.
func TestInitiateUploadRefusesAnotherTenantsConnection(t *testing.T) {
	client, _ := newFileTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should be made")
	}))
	client.tokens.Set(context.Background(), "dc-1", "access-token", time.Hour)

	_, err := client.InitiateUpload(context.Background(), testScope(), InitiateUploadRequest{
		ConnectionID: "dc-somebody-else", FileName: "a.txt", SizeBytes: 10,
	})
	assertProviderClass(t, err, policy.ClassValidation)
}

// TestInitiateUploadRequiresAConnectedAccount: without one there is no folder and no
// grant, and the answer must say "reconnect" rather than "unavailable", or the outage
// fallback would open for the wrong reason.
func TestInitiateUploadRequiresAConnectedAccount(t *testing.T) {
	client, resolver := newFileTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should be made for a disconnected account")
	}))
	resolver.connection.Status = StatusDisconnected

	_, err := client.InitiateUpload(context.Background(), testScope(), InitiateUploadRequest{FileName: "a.txt"})
	assertProviderClass(t, err, policy.ClassConsentRequired)
}

// TestFetchFileReadsDriveMetadata. Drive reports size as a JSON string because it
// exceeds float64 precision, so a naive number parse silently yields zero.
func TestFetchFileReadsDriveMetadata(t *testing.T) {
	client, _ := newFileTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"file-1","name":"notes.txt","mimeType":"text/plain",
			"size":"9007199254740993","md5Checksum":"abc",
			"trashed":false,"parents":["folder-1"],"webViewLink":"https://drive.google.com/x"
		}`))
	}))
	client.tokens.Set(context.Background(), "dc-1", "access-token", time.Hour)

	meta, err := client.FetchFile(context.Background(), testScope(), "file-1")
	if err != nil {
		t.Fatalf("FetchFile: %v", err)
	}
	if meta.ID != "file-1" || meta.SizeBytes != 9007199254740993 {
		t.Errorf("metadata = %+v; the size must survive the string encoding", meta)
	}
	if meta.MD5 != "abc" || meta.ParentID != "folder-1" {
		t.Errorf("metadata = %+v, want the checksum and parent", meta)
	}

	// The adapter must mark it as being in the folder Storvia manages.
	if converted := FileMetadataToModel(meta, "folder-1"); !converted.InAppFolder {
		t.Error("a file inside the app folder must be reported as such")
	}
	if converted := FileMetadataToModel(meta, "some-other-folder"); converted.InAppFolder {
		t.Error("a file outside the app folder must not be adopted")
	}
	if converted := FileMetadataToModel(meta, "folder-1"); converted.ChecksumAlgorithm != "md5" {
		t.Errorf("checksum algorithm = %q, want md5", converted.ChecksumAlgorithm)
	}
}

// TestFetchFileClassifiesAMissingFile so reconciliation can tell "nothing landed"
// from "cannot tell".
func TestFetchFileClassifiesAMissingFile(t *testing.T) {
	client, _ := newFileTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"errors":[{"reason":"notFound"}]}}`))
	}))
	client.tokens.Set(context.Background(), "dc-1", "access-token", time.Hour)

	_, err := client.FetchFile(context.Background(), testScope(), "gone")
	assertProviderClass(t, err, policy.ClassMissing)
}

// TestDownloadFileAsksForMedia checks the alt=media flag, without which Drive returns
// metadata instead of the bytes.
func TestDownloadFileAsksForMedia(t *testing.T) {
	var gotQuery string
	client, _ := newFileTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", "5")
		_, _ = w.Write([]byte("hello"))
	}))
	client.tokens.Set(context.Background(), "dc-1", "access-token", time.Hour)

	body, size, mime, err := client.DownloadFile(context.Background(), testScope(), "file-1")
	if err != nil {
		t.Fatalf("DownloadFile: %v", err)
	}
	defer body.Close()

	if !strings.Contains(gotQuery, "alt=media") {
		t.Errorf("query = %q, want alt=media", gotQuery)
	}
	if size != 5 || mime != "text/plain" {
		t.Errorf("size/mime = %d/%q, want 5/text/plain", size, mime)
	}
	content, _ := io.ReadAll(body)
	if string(content) != "hello" {
		t.Errorf("body = %q, want hello", content)
	}
}

// TestDeleteFileTreatsAMissingFileAsSuccess: deleting something already gone is the
// state the caller wanted, and reporting failure would block an idempotent delete.
func TestDeleteFileTreatsAMissingFileAsSuccess(t *testing.T) {
	var gotMethod string
	client, _ := newFileTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.WriteHeader(http.StatusNotFound)
	}))
	client.tokens.Set(context.Background(), "dc-1", "access-token", time.Hour)

	if err := client.DeleteFile(context.Background(), testScope(), "gone"); err != nil {
		t.Errorf("deleting an absent file must succeed: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", gotMethod)
	}
}

// TestDeleteFilePropagatesARealFailure: a 403 is not success and must not be hidden.
func TestDeleteFilePropagatesARealFailure(t *testing.T) {
	client, _ := newFileTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	client.tokens.Set(context.Background(), "dc-1", "access-token", time.Hour)

	err := client.DeleteFile(context.Background(), testScope(), "file-1")
	assertProviderClass(t, err, policy.ClassAuthorization)
}

// TestUploadStateReadsTheConfirmedOffset is what makes a resumable transfer safe to
// continue rather than restart.
func TestUploadStateReadsTheConfirmedOffset(t *testing.T) {
	client, _ := newFileTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Range"); got != "bytes */0" {
			t.Errorf("Content-Range = %q, want bytes */0", got)
		}
		// 308 with a Range header is Drive saying "I have this much".
		w.Header().Set("Range", "bytes=0-1023")
		w.WriteHeader(http.StatusPermanentRedirect)
	}))

	received, done, err := client.UploadState(context.Background(), "https://example.test/session")
	if err != nil {
		t.Fatalf("UploadState: %v", err)
	}
	if done {
		t.Error("a 308 means the upload is not finished")
	}
	if received != 1024 {
		t.Errorf("confirmed offset = %d, want 1024 (the range is inclusive)", received)
	}
}

// TestUploadStateReportsACompletedSession.
func TestUploadStateReportsACompletedSession(t *testing.T) {
	client, _ := newFileTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	_, done, err := client.UploadState(context.Background(), "https://example.test/session")
	if err != nil {
		t.Fatalf("UploadState: %v", err)
	}
	if !done {
		t.Error("a 200 means the file is complete")
	}
}

// TestUploadStateRequiresASessionURL keeps a blank capability from being turned into
// a request to an empty URL.
func TestUploadStateRequiresASessionURL(t *testing.T) {
	client, _ := newFileTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should be made")
	}))
	if _, _, err := client.UploadState(context.Background(), "  "); err == nil {
		t.Fatal("an empty session URL must be refused")
	}
}

// TestChunkSizeIsValid pins Drive's granularity rule, which is the one that bites
// partway through an upload that has already moved bytes.
func TestChunkSizeIsValid(t *testing.T) {
	if !ChunkSizeIsValid(DriveResumableChunkMultiple) {
		t.Error("the base granularity must be valid")
	}
	if !ChunkSizeIsValid(DriveResumableChunkMultiple * 4) {
		t.Error("a multiple of the granularity must be valid")
	}
	for _, bad := range []int{0, -1, 1, DriveResumableChunkMultiple - 1, DriveResumableChunkMultiple + 1} {
		if ChunkSizeIsValid(bad) {
			t.Errorf("%d must be rejected; Drive requires a 256 KiB multiple", bad)
		}
	}
}

// TestDeclaredChunkSizeIsAlwaysAccepted covers the small-file case, where the file is
// one sub-granularity chunk.
func TestDeclaredChunkSizeIsAlwaysAccepted(t *testing.T) {
	client := &Client{}

	if got := client.declaredChunkSize(100); got != 100 {
		t.Errorf("chunk size for 100 bytes = %d, want 100", got)
	}
	if got := client.declaredChunkSize(0); !ChunkSizeIsValid(got) {
		t.Errorf("chunk size for an unknown total = %d, want a valid granularity", got)
	}
	if got := client.declaredChunkSize(10 << 20); !ChunkSizeIsValid(got) {
		t.Errorf("chunk size for 10 MiB = %d, want a valid granularity", got)
	}
}

// assertProviderClass fails unless err is a provider error of the expected class.
func assertProviderClass(t *testing.T, err error, want policy.FailureClass) {
	t.Helper()
	if err == nil {
		t.Fatalf("want a %s error, got nil", want)
	}
	var providerErr *policy.ProviderError
	if !asProviderError(err, &providerErr) {
		t.Fatalf("error %v is not a provider error", err)
	}
	if providerErr.Class != want {
		t.Errorf("class = %q, want %q (detail: %s)", providerErr.Class, want, providerErr.SafeDetail)
	}
}

func asProviderError(err error, target **policy.ProviderError) bool {
	return errors.As(err, target)
}

// TestFetchSubjectUsesDriveAboutNotOpenIDConnect is the regression guard for a bug
// that made every connection fail.
//
// The subject used to be fetched from OpenID Connect's userinfo endpoint, which
// requires an `openid` grant. Storvia requests only `drive.file`, by design, so the
// access token was always rejected there with a 401 — after the user had already
// consented, which is the worst moment to discover it.
//
// Asking Drive's own `about` endpoint keeps identification inside `drive.file`, so it
// costs no additional permission.
func TestFetchSubjectUsesDriveAboutNotOpenIDConnect(t *testing.T) {
	var gotPath, gotQuery, gotAuth string
	client, _ := newFileTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotAuth = r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user":{"permissionId":"perm-123","emailAddress":"a@b.test","displayName":"A"}}`))
	}))
	client.tokens.Set(context.Background(), "dc-1", "access-token", time.Hour)

	subject, err := client.fetchSubject(context.Background(), "access-token")
	if err != nil {
		t.Fatalf("fetchSubject: %v", err)
	}

	if gotPath != "/drive/v3/about" {
		t.Errorf("path = %q, want /drive/v3/about. "+
			"The OpenID Connect endpoint needs an openid grant that Storvia never requests.", gotPath)
	}
	if gotPath == "/drive/v3/files/about" {
		t.Error("the about endpoint is not under /files")
	}
	if !strings.Contains(gotQuery, "fields=user") {
		t.Errorf("query = %q, want fields=user", gotQuery)
	}
	if gotAuth != "Bearer access-token" {
		t.Errorf("authorization = %q", gotAuth)
	}
	if subject != "perm-123" {
		t.Errorf("subject = %q, want permission-123", subject)
	}
}

// TestFetchSubjectReportsWhyItFailed keeps the provider's reason. Without this the
// only thing an operator sees is "could not be completed", which is how the scope
// mismatch went uninvestigated.
func TestFetchSubjectReportsWhyItFailed(t *testing.T) {
	client, _ := newFileTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"status":"UNAUTHENTICATED","message":"Invalid Credentials"}}`))
	}))

	_, err := client.fetchSubject(context.Background(), "bad-token")
	if err == nil {
		t.Fatal("a rejected token must be an error")
	}
	if !strings.Contains(err.Error(), "UNAUTHENTICATED") || !strings.Contains(err.Error(), "Invalid Credentials") {
		t.Errorf("error = %v, want it to carry google's reason", err)
	}
}

// TestParseTokenResponseReadsGoogleJSON is the regression guard for a bug that broke
// every single exchange.
//
// Google's token endpoint answers with `application/json`. The code parsed it with
// url.ParseQuery, which is form encoding, so a successful response produced an empty
// value set and the exchange failed with "the token response contained no access token"
// while the token sat in the body in plain sight.
//
// It also broke refresh, because both paths shared the parser.
func TestParseTokenResponseReadsGoogleJSON(t *testing.T) {
	body := []byte(`{
		"access_token": "ya29.a0AfH6B",
		"expires_in": 3599,
		"refresh_token": "1//refresh",
		"scope": "https://www.googleapis.com/auth/drive.file",
		"token_type": "Bearer"
	}`)

	values, err := parseTokenResponse(body)
	if err != nil {
		t.Fatalf("parseTokenResponse: %v", err)
	}

	if values.Get("access_token") != "ya29.a0AfH6B" {
		t.Errorf("access_token = %q, want the value from the json body", values.Get("access_token"))
	}
	// The refresh token is the whole point of the flow; losing it silently would make
	// the connection work once and then fail forever.
	if values.Get("refresh_token") != "1//refresh" {
		t.Errorf("refresh_token = %q, want 1//refresh", values.Get("refresh_token"))
	}
	// A number must arrive as a string, because that is what the duration parser
	// downstream expects.
	if values.Get("expires_in") != "3599" {
		t.Errorf("expires_in = %q, want the number rendered as 3599", values.Get("expires_in"))
	}
	if values.Get("token_type") != "Bearer" {
		t.Errorf("token_type = %q", values.Get("token_type"))
	}
}

// TestParseTokenResponseStillAcceptsFormEncoding keeps the spec-permitted alternative
// working, so this is a fallback rather than a replacement.
func TestParseTokenResponseStillAcceptsFormEncoding(t *testing.T) {
	values, err := parseTokenResponse([]byte("access_token=abc123&expires_in=3599&token_type=Bearer"))
	if err != nil {
		t.Fatalf("form encoding must still be accepted: %v", err)
	}
	if values.Get("access_token") != "abc123" {
		t.Errorf("access_token = %q, want abc123", values.Get("access_token"))
	}
}

// TestEnsureRootFolderReusesAnExistingFolder is the property that keeps a user from
// ending up with several folders called Storvia.
func TestEnsureRootFolderReusesAnExistingFolder(t *testing.T) {
	var created int
	client, _ := newFileTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			created++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"folder-new"}`))
			return
		}
		// The query must filter by name and type, or the listing returns every
		// folder in the account. The decoded value is checked because the raw
		// query is percent-encoded.
		filter := r.URL.Query().Get("q")
		if !strings.Contains(filter, "Storvia") || !strings.Contains(filter, folderMimeType) {
			t.Errorf("filter = %q, want a name and mimeType filter", filter)
		}
		if !strings.Contains(filter, "trashed = false") {
			t.Errorf("filter = %q, want trashed folders excluded; reusing one would "+
				"write into the user's trash", filter)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"files":[{"id":"folder-1","name":"Storvia"}]}`))
	}))

	state, err := client.EnsureRootFolder(context.Background(), testScope())
	if err != nil {
		t.Fatalf("EnsureRootFolder: %v", err)
	}
	if state.FolderID != "folder-1" {
		t.Errorf("folder = %q, want the existing folder-1", state.FolderID)
	}
	if created != 0 {
		t.Errorf("a folder was created (%d) even though one already existed", created)
	}
}

// TestEnsureRootFolderCreatesWhenAbsent.
func TestEnsureRootFolderCreatesWhenAbsent(t *testing.T) {
	var createName, createMime string
	client, _ := newFileTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			var payload map[string]string
			_ = json.NewDecoder(r.Body).Decode(&payload)
			createName, createMime = payload["name"], payload["mimeType"]
			_, _ = w.Write([]byte(`{"id":"folder-new"}`))
			return
		}
		_, _ = w.Write([]byte(`{"files":[]}`))
	}))

	state, err := client.EnsureRootFolder(context.Background(), testScope())
	if err != nil {
		t.Fatalf("EnsureRootFolder: %v", err)
	}
	if state.FolderID != "folder-new" {
		t.Errorf("folder = %q, want the newly created folder-new", state.FolderID)
	}
	if createMime != folderMimeType {
		t.Errorf("mimeType = %q, want %q", createMime, folderMimeType)
	}
	if createName != "Storvia" {
		t.Errorf("name = %q, want Storvia", createName)
	}
}

// TestEnsureRootFolderRefusesToChooseBetweenDuplicates is the recovery policy. Two
// folders called Storvia means picking one would split the user's files, and creating
// another would make the ambiguity permanent.
func TestEnsureRootFolderRefusesToChooseBetweenDuplicates(t *testing.T) {
	created := false
	client, _ := newFileTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			created = true
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"files":[{"id":"folder-b","name":"Storvia"},{"id":"folder-a","name":"Storvia"}]}`))
	}))

	state, err := client.EnsureRootFolder(context.Background(), testScope())
	if err != nil {
		t.Fatalf("an ambiguous name must be reported, not treated as an error here: %v", err)
	}
	if !state.Ambiguous {
		t.Error("two folders named Storvia must be reported as ambiguous")
	}
	if state.FolderID != "" {
		t.Errorf("folder = %q, want none chosen", state.FolderID)
	}
	if len(state.Candidates) != 2 || state.Candidates[0] != "folder-a" {
		t.Errorf("candidates = %v, want them sorted for the conversation", state.Candidates)
	}
	if created {
		t.Error("a third folder must not be created while the name is ambiguous")
	}
}

// TestFindRootFolderNeverCreates. Recovery is explicit; silently creating a replacement
// would move a boundary the user controls without telling them.
func TestFindRootFolderNeverCreates(t *testing.T) {
	client, _ := newFileTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Error("FindRootFolder must never create a folder")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"files":[]}`))
	}))

	state, err := client.FindRootFolder(context.Background(), testScope())
	if err != nil {
		t.Fatalf("FindRootFolder: %v", err)
	}
	if state.Exists || state.FolderID != "" {
		t.Errorf("state = %+v, want no folder found", state)
	}
}

// TestFolderWorkToleratesAMissingRootFolderID matters because finding or creating that
// folder is precisely how the id gets set; refusing first would be a deadlock.
func TestFolderWorkToleratesAMissingRootFolderID(t *testing.T) {
	client, resolver := newFileTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"files":[{"id":"folder-1","name":"Storvia"}]}`))
	}))
	resolver.connection.RootFolderID = ""

	state, err := client.FindRootFolder(context.Background(), testScope())
	if err != nil {
		t.Fatalf("a connection with no folder id must still be usable for folder work: %v", err)
	}
	if state.FolderID != "folder-1" {
		t.Errorf("folder = %q, want folder-1", state.FolderID)
	}
}

// TestStartPageTokenIsFetchedWhenNoCursorExists is the bug this guards.
//
// Drive requires pageToken on every changes.list call, including the first. Sending
// the list without one returns 400 "Required parameter: pageToken", so a first poll
// would fail forever and the change log would never advance.
func TestStartPageTokenIsFetchedWhenNoCursorExists(t *testing.T) {
	var sawStartRequest bool
	client, _ := newFileTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/startPageToken"):
			sawStartRequest = true
			_, _ = w.Write([]byte(`{"startPageToken":"start-1"}`))
		case r.URL.Path == "/drive/v3/changes":
			if got := r.URL.Query().Get("pageToken"); got != "start-1" {
				t.Errorf("pageToken = %q, want the start token just minted", got)
			}
			_, _ = w.Write([]byte(`{"newStartPageToken":"next-1","changes":[]}`))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
		}
	}))

	page, err := client.ListChanges(context.Background(), "dc-1", "", 10)
	if err != nil {
		t.Fatalf("ListChanges: %v", err)
	}
	if !sawStartRequest {
		t.Error("no start page token was requested")
	}
	if page.NewestCursor != "next-1" {
		t.Errorf("newest cursor = %q, want next-1", page.NewestCursor)
	}
}

// TestListChangesUsesTheStoredCursor checks the incremental path: once a cursor is
// stored it is sent rather than a fresh start token.
func TestListChangesUsesTheStoredCursor(t *testing.T) {
	client, resolver := newFileTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/startPageToken") {
			t.Error("a start token was minted even though a cursor was stored")
			return
		}
		if got := r.URL.Query().Get("pageToken"); got != "stored-1" {
			t.Errorf("pageToken = %q, want the stored cursor", got)
		}
		_, _ = w.Write([]byte(`{"changes":[{"fileId":"f1","removed":true}]}`))
	}))
	cursor := "stored-1"
	resolver.connection.ChangesCursor = &cursor

	page, err := client.ListChanges(context.Background(), "dc-1", "stored-1", 10)
	if err != nil {
		t.Fatalf("ListChanges: %v", err)
	}
	if len(page.Changes) != 1 || page.Changes[0].FileID != "f1" || !page.Changes[0].Removed {
		t.Errorf("changes = %+v, want one removal for f1", page.Changes)
	}
}

// TestExpiredPageTokenErrorKeepsItsBody guards a bug in the diagnostic itself:
// isExpiredPageToken reads the response body, and anything that classified the error
// afterwards saw an empty body, so Google's actual message was lost.
func TestExpiredPageTokenErrorKeepsItsBody(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"Invalid Value: pageToken"}}`)),
	}

	if !isExpiredPageToken(resp) {
		t.Fatal("a stale page token must be recognised")
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "pageToken") {
		t.Errorf("the body was consumed and not restored: %q", body)
	}
}

// TestInitiateSendsOnlyWritableFields is the regression test for a real 403.
//
// Drive rejected every resumable initiation with
//
//	403 fieldNotWritable "The resource body includes fields which are not directly writable"
//
// because the create metadata carried `size`. `size` is a computed field on a Drive
// file: it is returned by the API and cannot be set. The declared length belongs in
// the X-Upload-Content-Length header.
//
// This went undetected because every other test asserts on what Storvia sent rather
// than on what Drive accepts. Encoding the writable-field list here means the next
// person to add a convenient field trips a test instead of a live account.
func TestInitiateSendsOnlyWritableFields(t *testing.T) {
	var metadata map[string]any

	client, _ := newFileTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/upload/drive/v3/files" {
			_ = json.NewDecoder(r.Body).Decode(&metadata)
			w.Header().Set("Location", "https://www.googleapis.com/upload/session/xyz")
			w.WriteHeader(http.StatusOK)
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}))

	_, err := client.InitiateUpload(context.Background(), testScope(), InitiateUploadRequest{
		FileName:  "notes.txt",
		MimeType:  "text/plain",
		SizeBytes: 4096,
	})
	if err != nil {
		t.Fatalf("InitiateUpload: %v", err)
	}

	// Drive rejects a create that carries any of these. Each was at some point a
	// plausible-looking thing to send.
	for _, notWritable := range []string{"size", "id", "createdTime", "modifiedTime",
		"md5Checksum", "webViewLink", "owners", "quotaBytesUsed", "version"} {
		if _, present := metadata[notWritable]; present {
			t.Errorf("the create metadata carries %q, which drive will refuse with "+
				"fieldNotWritable; the length belongs in X-Upload-Content-Length",
				notWritable)
		}
	}

	for _, required := range []string{"name", "parents", "mimeType"} {
		if _, present := metadata[required]; !present {
			t.Errorf("the create metadata is missing %q", required)
		}
	}
}

// TestInitiateDeclaresLengthInTheHeader. The length is still communicated -- just not
// as a body field -- so this pins where it went rather than only what left.
func TestInitiateDeclaresLengthInTheHeader(t *testing.T) {
	var declared string

	client, _ := newFileTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/upload/drive/v3/files" {
			declared = r.Header.Get("X-Upload-Content-Length")
			w.Header().Set("Location", "https://www.googleapis.com/upload/session/xyz")
			w.WriteHeader(http.StatusOK)
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}))

	if _, err := client.InitiateUpload(context.Background(), testScope(), InitiateUploadRequest{
		FileName: "notes.txt", MimeType: "text/plain", SizeBytes: 4096,
	}); err != nil {
		t.Fatalf("InitiateUpload: %v", err)
	}

	if declared != "4096" {
		t.Errorf("X-Upload-Content-Length = %q, want 4096; drive must be told the "+
			"expected length by header, since it cannot be set on the resource",
			declared)
	}
}

// TestConnectStoresTheAccountProfile is the bug this prevents.
//
// The console could only ever show a configured id like "user_dev_admin" because the
// display name, email and picture were fetched on every connect and thrown away --
// Google's `about` call had all three on the wire.
//
// The test asserts they reach storage, because a field that is fetched and discarded is
// indistinguishable from one that was never requested.
func TestConnectStoresTheAccountProfile(t *testing.T) {
	var resolver *stubResolver
	client, r := newFileTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.URL.Path == "/token":
			_, _ = w.Write([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":3600}`))
		case req.URL.Path == "/drive/v3/about":
			_, _ = w.Write([]byte(`{"user":{
				"permissionId":"perm-1",
				"emailAddress":"shovan@example.com",
				"displayName":"Shovan B",
				"photoLink":"https://lh3.googleusercontent.com/a/abc123"}}`))
		case req.URL.Path == "/drive/v3/files":
			_, _ = w.Write([]byte(`{"id":"f1","name":"n"}`))
		default:
			t.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
		}
	}))
	resolver = r

	// The state has to be remembered first, or the exchange is refused before it
	// reaches Google -- which is the whole point of the browser binding.
	client.RememberState("the-state", "the-binding")

	_, err := client.ExchangeWithRedirect(context.Background(), Scope{TenantID: "t1", UserID: "u1"},
		"the-code", "the-state", "the-binding", "http://127.0.0.1:8080/admin/oauth/callback")
	if err != nil {
		t.Fatalf("ExchangeWithRedirect: %v", err)
	}

	// Identity is still the stable identifier, not the email.
	if resolver.saved == nil || len(resolver.saved) == 0 {
		t.Fatal("nothing was saved")
	}
	if got := resolver.saved[len(resolver.saved)-1].Subject; got != "perm-1" {
		t.Errorf("subject = %q, want perm-1: the email must not become the identity", got)
	}

	last := resolver.saved[len(resolver.saved)-1]
	if last.DisplayName != "Shovan B" {
		t.Errorf("display name = %q, want the value Google reported", last.DisplayName)
	}
	if last.Email != "shovan@example.com" {
		t.Errorf("email = %q, want the value Google reported", last.Email)
	}
	if last.PictureURL != "https://lh3.googleusercontent.com/a/abc123" {
		t.Errorf("picture = %q, want the value Google reported", last.PictureURL)
	}
}

// TestAccountProfileIsNotIdentity is the guard on the boundary.
//
// An email address can change and a display name is not unique. If either ever became
// something a connection is keyed on, an account could be impersonated or orphaned by a
// rename.
func TestAccountProfileIsNotIdentity(t *testing.T) {
	var resolver *stubResolver
	client, r := newFileTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch req.URL.Path {
		case "/token":
			_, _ = w.Write([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":3600}`))
		case "/drive/v3/about":
			// An account with no display name and no photo. Neither may fail the
			// connect: they are cosmetic.
			_, _ = w.Write([]byte(`{"user":{"permissionId":"perm-2","emailAddress":"bare@example.com"}}`))
		default:
			t.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
		}
	}))
	resolver = r

	client.RememberState("s", "b")
	if _, err := client.ExchangeWithRedirect(context.Background(), Scope{TenantID: "t1", UserID: "u1"},
		"c", "s", "b", "http://127.0.0.1:8080/admin/oauth/callback"); err != nil {
		t.Fatalf("an account with no display name or picture must still connect: %v", err)
	}

	last := resolver.saved[len(resolver.saved)-1]
	if last.Subject != "perm-2" {
		t.Errorf("subject = %q, want perm-2", last.Subject)
	}
	if last.DisplayName != "" || last.PictureURL != "" {
		t.Errorf("expected empty display fields, got %q / %q", last.DisplayName, last.PictureURL)
	}
}
