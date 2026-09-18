// Package httpapi is the HTTP transport: routing, request validation, and
// response rendering.
package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/lemonn/typecode-middleware/internal/apperror"
	"github.com/lemonn/typecode-middleware/internal/httpmw"
)

const contentTypeJSON = "application/json; charset=utf-8"

// errorEnvelope is the single shape every failure takes, so clients have one
// thing to parse regardless of which layer failed.
type errorEnvelope struct {
	Error     errorBody `json:"error"`
	RequestID string    `json:"request_id,omitempty"`
}

type errorBody struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

// writeJSON renders a successful response.
//
// The payload is marshalled before anything is written, so an encoding failure
// becomes a clean 500 rather than a half-sent body with a 200 status already
// committed.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, payload any, log *slog.Logger) {
	body, err := json.Marshal(payload)
	if err != nil {
		log.ErrorContext(r.Context(), "failed to encode response",
			slog.String("request_id", httpmw.RequestIDFrom(r.Context())),
			slog.String("error", err.Error()))
		writeError(w, r, apperror.Internal(err), log)
		return
	}

	w.Header().Set("Content-Type", contentTypeJSON)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// writeError renders a failure. Any error is normalised first, so an unexpected
// error type becomes a 500 with a generic message rather than leaking internals.
func writeError(w http.ResponseWriter, r *http.Request, err error, log *slog.Logger) {
	appErr := apperror.From(err)

	// Server-side faults carry a cause worth recording; client mistakes do not.
	if appErr.Status >= http.StatusInternalServerError {
		log.ErrorContext(r.Context(), "request failed",
			slog.String("request_id", httpmw.RequestIDFrom(r.Context())),
			slog.String("code", string(appErr.Code)),
			slog.String("error", appErr.Error()))
	}

	envelope := errorEnvelope{
		Error: errorBody{
			Code:    string(appErr.Code),
			Message: appErr.Message,
			Fields:  appErr.Fields,
		},
		RequestID: httpmw.RequestIDFrom(r.Context()),
	}

	body, err := json.Marshal(envelope)
	if err != nil {
		http.Error(w, `{"error":{"code":"internal_error","message":"internal server error"}}`,
			http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", contentTypeJSON)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(appErr.Status)
	_, _ = w.Write(body)
}
