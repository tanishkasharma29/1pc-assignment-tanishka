// Package apperror defines the single error type that crosses layer boundaries.
//
// Errors carry two audiences: Message is safe to return to a client, while the
// wrapped err is for operators only and is never serialised. Handlers therefore
// never have to decide what is safe to leak — the constructor already did.
package apperror

import (
	"errors"
	"fmt"
	"net/http"
)

// Code is a stable, machine-readable error identifier. Clients may branch on
// these; they are part of the API contract and must not be renamed casually.
type Code string

const (
	CodeBadRequest      Code = "bad_request"
	CodeNotFound        Code = "not_found"
	CodeUpstream        Code = "upstream_error"
	CodeUpstreamTimeout Code = "upstream_timeout"
	CodeBudget          Code = "upstream_budget_exhausted"
	CodeRateLimited     Code = "rate_limited"
	CodeInternal        Code = "internal_error"
)

// Error is the canonical application error.
type Error struct {
	Code    Code
	Status  int
	Message string            // client-safe
	Fields  map[string]string // per-field validation detail, client-safe
	err     error             // operator-only cause, never serialised
}

func (e *Error) Error() string {
	if e.err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.err }

// WithField attaches field-level validation detail and returns the receiver so
// it can be chained at a return site.
func (e *Error) WithField(field, reason string) *Error {
	if e.Fields == nil {
		e.Fields = make(map[string]string, 1)
	}
	e.Fields[field] = reason
	return e
}

// BadRequest is for input the proxy rejects at the edge. Crucially, these cost
// nothing upstream: the request is refused before any billable call is made.
func BadRequest(msg string) *Error {
	return &Error{Code: CodeBadRequest, Status: http.StatusBadRequest, Message: msg}
}

// NotFound reports a resource the upstream does not have. This is a valid,
// cacheable answer — not a failure — because the upstream data is immutable.
func NotFound(msg string) *Error {
	return &Error{Code: CodeNotFound, Status: http.StatusNotFound, Message: msg}
}

// Upstream reports a failed fetch. Deliberately 502: the client's request was
// well-formed, so the fault is ours or the upstream's, never theirs.
func Upstream(cause error) *Error {
	return &Error{
		Code:    CodeUpstream,
		Status:  http.StatusBadGateway,
		Message: "upstream service returned an unexpected response",
		err:     cause,
	}
}

// UpstreamTimeout is separated from Upstream because it is operationally a very
// different signal: latency rather than breakage.
func UpstreamTimeout(cause error) *Error {
	return &Error{
		Code:    CodeUpstreamTimeout,
		Status:  http.StatusGatewayTimeout,
		Message: "upstream service did not respond in time",
		err:     cause,
	}
}

// BudgetExhausted is returned when the global upstream call budget is spent. It
// is the last line of defence in the cost model: we would rather fail a cache
// miss than spend money we have decided not to spend.
func BudgetExhausted() *Error {
	return &Error{
		Code:    CodeBudget,
		Status:  http.StatusServiceUnavailable,
		Message: "upstream call budget exhausted; try again later",
	}
}

// RateLimited is returned when a single client exceeds its own allowance.
func RateLimited() *Error {
	return &Error{
		Code:    CodeRateLimited,
		Status:  http.StatusTooManyRequests,
		Message: "too many requests",
	}
}

// Internal is the catch-all. Its Message is intentionally uninformative; the
// cause reaches the logs, never the client.
func Internal(cause error) *Error {
	return &Error{
		Code:    CodeInternal,
		Status:  http.StatusInternalServerError,
		Message: "internal server error",
		err:     cause,
	}
}

// From normalises any error into an *Error so the response writer has exactly
// one shape to handle. Unrecognised errors become Internal, which fails closed.
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var appErr *Error
	if errors.As(err, &appErr) {
		return appErr
	}
	return Internal(err)
}
