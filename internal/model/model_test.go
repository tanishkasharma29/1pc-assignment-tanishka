package model

import (
	"encoding/json"
	"testing"
)

// The fixtures below are the exact payloads the live upstream returned when this
// service was designed.

func TestPostDecodesUpstreamPayload(t *testing.T) {
	t.Parallel()
	raw := `{"userId":1,"id":1,"title":"sunt aut facere","body":"quia et\nsuscipit"}`

	var post Post
	if err := json.Unmarshal([]byte(raw), &post); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if post.UserID != 1 || post.ID != 1 || post.Title != "sunt aut facere" {
		t.Fatalf("decoded = %+v", post)
	}
	if post.Body != "quia et\nsuscipit" {
		t.Errorf("embedded newlines must survive decoding, got %q", post.Body)
	}
	if err := post.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestTodoDecodesUpstreamPayload(t *testing.T) {
	t.Parallel()
	raw := `{"userId":1,"id":1,"title":"delectus aut autem","completed":false}`

	var todo Todo
	if err := json.Unmarshal([]byte(raw), &todo); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if todo.Completed {
		t.Error("completed should be false")
	}
	if err := todo.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// TestValidateRejectsDegradedPayloads guards the cache: an entry stored once may
// be served for a very long time, so a malformed upstream response must be
// caught before it can be persisted.
func TestValidateRejectsDegradedPayloads(t *testing.T) {
	t.Parallel()

	for name, post := range map[string]Post{
		"zero id":      {UserID: 1, ID: 0},
		"negative id":  {UserID: 1, ID: -5},
		"missing user": {UserID: 0, ID: 1},
	} {
		if err := post.Validate(); err == nil {
			t.Errorf("Post %s: expected a validation error", name)
		}
	}

	for name, todo := range map[string]Todo{
		"zero id":      {UserID: 1, ID: 0},
		"missing user": {UserID: 0, ID: 1},
	} {
		if err := todo.Validate(); err == nil {
			t.Errorf("Todo %s: expected a validation error", name)
		}
	}
}

// TestCompletedFalseIsValid: false is both a legitimate value and the zero
// value, so it must not be treated as missing.
func TestCompletedFalseIsValid(t *testing.T) {
	t.Parallel()
	if err := (Todo{UserID: 1, ID: 1, Completed: false}).Validate(); err != nil {
		t.Errorf("completed=false is a valid todo: %v", err)
	}
}
