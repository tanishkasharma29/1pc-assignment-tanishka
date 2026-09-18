package apperror

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestConstructorsMapToStatuses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		err        *Error
		wantStatus int
		wantCode   Code
	}{
		{"bad request", BadRequest("nope"), http.StatusBadRequest, CodeBadRequest},
		{"not found", NotFound("gone"), http.StatusNotFound, CodeNotFound},
		{"upstream", Upstream(errors.New("x")), http.StatusBadGateway, CodeUpstream},
		{"timeout", UpstreamTimeout(errors.New("x")), http.StatusGatewayTimeout, CodeUpstreamTimeout},
		{"budget", BudgetExhausted(), http.StatusServiceUnavailable, CodeBudget},
		{"rate limited", RateLimited(), http.StatusTooManyRequests, CodeRateLimited},
		{"internal", Internal(errors.New("x")), http.StatusInternalServerError, CodeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err.Status != tc.wantStatus {
				t.Errorf("Status = %d, want %d", tc.err.Status, tc.wantStatus)
			}
			if tc.err.Code != tc.wantCode {
				t.Errorf("Code = %q, want %q", tc.err.Code, tc.wantCode)
			}
		})
	}
}

// TestInternalDetailNeverReachesTheMessage: the cause belongs in the logs, not
// in a client response.
func TestInternalDetailNeverReachesTheMessage(t *testing.T) {
	t.Parallel()
	secret := errors.New("connection to db-prod-7 refused: password=hunter2")
	err := Internal(secret)

	if strings.Contains(err.Message, "hunter2") || strings.Contains(err.Message, "db-prod-7") {
		t.Fatalf("Message leaked the cause: %q", err.Message)
	}
	if !errors.Is(err, secret) {
		t.Error("the cause must remain reachable through Unwrap for logging")
	}
}

func TestWithFieldAccumulates(t *testing.T) {
	t.Parallel()
	err := BadRequest("invalid").WithField("id", "must be an integer").WithField("userId", "out of range")

	if len(err.Fields) != 2 {
		t.Fatalf("Fields = %v, want 2 entries", err.Fields)
	}
	if err.Fields["id"] != "must be an integer" {
		t.Errorf("Fields[id] = %q", err.Fields["id"])
	}
}

func TestFromNormalises(t *testing.T) {
	t.Parallel()

	if got := From(nil); got != nil {
		t.Errorf("From(nil) = %v, want nil", got)
	}

	original := NotFound("missing")
	if got := From(original); got != original {
		t.Error("From must return an existing *Error unchanged")
	}

	// Wrapped app errors must still be recognised, or a handler that adds
	// context would silently downgrade a 404 into a 500.
	wrapped := fmt.Errorf("while fetching: %w", NotFound("missing"))
	if got := From(wrapped); got.Status != http.StatusNotFound {
		t.Errorf("wrapped error resolved to status %d, want 404", got.Status)
	}

	// An unknown error fails closed.
	if got := From(errors.New("surprise")); got.Status != http.StatusInternalServerError {
		t.Errorf("unknown error resolved to status %d, want 500", got.Status)
	}
}

func TestErrorString(t *testing.T) {
	t.Parallel()
	if got := BadRequest("bad").Error(); !strings.Contains(got, "bad_request") {
		t.Errorf("Error() = %q, want the code included", got)
	}
	if got := Upstream(errors.New("boom")).Error(); !strings.Contains(got, "boom") {
		t.Errorf("Error() = %q, want the cause included for operators", got)
	}
}
