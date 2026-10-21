// Package httpapi is the transport layer: routes, decoding, middleware, and the
// single place where an error becomes a response body.
//
// Handlers here stay thin. They decode, call a use case, and write a result. Any
// policy decision, tenant check, or provider interaction belongs further in.
package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/shovan04/storvia-project/internal/apperr"
	"github.com/shovan04/storvia-project/internal/logger"
)

// Problem is the RFC 9457 problem details body.
//
// Every failure this API returns uses this shape, including 4xx bodies, so a
// client has exactly one error format to parse. A bespoke envelope per route is
// what forces SDK consumers to special-case every endpoint.
type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
	// Instance is the request path, which lets a user report a failure with
	// enough context to reproduce it.
	Instance string `json:"instance,omitempty"`
	// RequestID correlates the failure with the server log line.
	RequestID string `json:"request_id,omitempty"`
	// Fields carries operator-authored context such as the failing field name.
	// It never carries raw user input or provider payloads.
	Fields map[string]string `json:"fields,omitempty"`
}

// errorResponse is the success-shaped error envelope. It exists so a Go client
// can decode one struct for both outcomes.
type errorResponse struct {
	Error Problem `json:"error"`
}

// WriteJSON writes v as JSON with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status is already committed, so this can only be logged.
		slog.Default().Error("httpapi: encode response", "error", err.Error())
	}
}

// WriteProblem renders err as an RFC 9457 body.
//
// This is the global error handler: every handler returns an error-shaped value
// and funnels through here, so no route can invent its own error format and no
// internal cause can leak by accident.
func WriteProblem(w http.ResponseWriter, r *http.Request, err error) {
	appErr := normalize(err)
	status := appErr.Status()
	problem := Problem{
		Type:      appErr.ProblemType(),
		Title:     appErr.Title,
		Status:    status,
		Detail:    appErr.Detail,
		Instance:  r.URL.Path,
		RequestID: logger.CorrelationID(r.Context()),
		Fields:    appErr.Fields,
	}

	logLevel := slog.LevelWarn
	if status >= http.StatusInternalServerError {
		logLevel = slog.LevelError
	}

	requestLogger := logger.With(r.Context(), slog.Default()).With("http_error", logLevel,
		"kind", string(appErr.Kind),
		"status", status,
		"method", r.Method,
		"path", r.URL.Path,
	)
	// The wrapped cause is logged for internal kinds and deliberately omitted
	// otherwise: a provider error body can contain identifiers or file names.
	if appErr.Cause != nil {
		requestLogger = requestLogger.With("cause", appErr.Cause.Error())
	}
	if status >= http.StatusInternalServerError {
		requestLogger.LogAttrs(r.Context(), logLevel, "request failed")
	} else {
		requestLogger.LogAttrs(r.Context(), logLevel, "request rejected")
	}

	WriteJSON(w, status, errorResponse{Error: problem})
}

// normalize coerces any error into an *apperr.Error.
//
// An error that is not one of ours is reported as internal with a fixed detail,
// so a bare fmt.Errorf from a deep package cannot become a client-visible string.
func normalize(err error) *apperr.Error {
	if err == nil {
		return apperr.Internal("an unexpected error occurred")
	}
	var appErr *apperr.Error
	if errors.As(err, &appErr) {
		return appErr
	}
	return apperr.Wrap(err, apperr.KindInternal,
		"Internal error", "an unexpected error occurred")
}

// DecodeJSON reads a JSON request body into dst, rejecting unknown fields.
//
// Strict decoding matters for an upload request: a client that misspells
// "size_bytes" must get an error rather than a zero value that silently becomes
// an unlimited upload.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	contentType := r.Header.Get("Content-Type")
	if contentType != "" && !strings.HasPrefix(contentType, "application/json") {
		return apperr.Validation("Content-Type must be application/json")
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		switch {
		case errors.As(err, &syntaxErr):
			return apperr.Validation("request body is not valid JSON").
				WithField("offset", strconv.FormatInt(syntaxErr.Offset, 10))
		case errors.As(err, &typeErr):
			return apperr.Validation("request body has a field of the wrong type").
				WithField("field", typeErr.Field)
		case errors.Is(err, http.ErrBodyReadAfterClose), errors.Is(err, errBodyTooLarge):
			return apperr.Validation("request body is too large")
		case err.Error() == "EOF":
			return apperr.Validation("request body is empty")
		default:
			return apperr.Validation("request body does not match the expected shape")
		}
	}
	return nil
}

// maxRequestBodySize bounds metadata request bodies. File bytes never travel
// through a JSON body; they go through the chunk or presigned paths.
const maxRequestBodySize = 1 << 20

// maxRequestBody is the reader limit used by DecodeJSON.
var maxRequestBody = int64(maxRequestBodySize)

var errBodyTooLarge = errors.New("httpapi: request body too large")
