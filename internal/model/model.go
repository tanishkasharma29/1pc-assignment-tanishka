// Package model holds the upstream resource types.
//
// We decode into these rather than proxying opaque bytes so that a change in the
// upstream contract surfaces as a clear error here instead of as malformed data
// reaching clients. The decode cost is paid only on a cache miss, which the rest
// of the system exists to make rare.
package model

import "fmt"

// Post mirrors https://jsonplaceholder.typicode.com/posts/:id
type Post struct {
	UserID int    `json:"userId"`
	ID     int    `json:"id"`
	Title  string `json:"title"`
	Body   string `json:"body"`
}

// Validate asserts the invariants we observed on the live upstream. It guards
// against silently caching a degraded response (for example an error page that
// happens to be valid JSON) for the lifetime of an immutable entry.
func (p Post) Validate() error {
	if p.ID <= 0 {
		return fmt.Errorf("post: id must be positive, got %d", p.ID)
	}
	if p.UserID <= 0 {
		return fmt.Errorf("post: userId must be positive, got %d", p.UserID)
	}
	return nil
}

// Todo mirrors https://jsonplaceholder.typicode.com/todos/:id
type Todo struct {
	UserID    int    `json:"userId"`
	ID        int    `json:"id"`
	Title     string `json:"title"`
	Completed bool   `json:"completed"`
}

// Validate mirrors Post.Validate. Completed is deliberately unchecked: false is
// both a valid value and the zero value, so it carries no signal.
func (t Todo) Validate() error {
	if t.ID <= 0 {
		return fmt.Errorf("todo: id must be positive, got %d", t.ID)
	}
	if t.UserID <= 0 {
		return fmt.Errorf("todo: userId must be positive, got %d", t.UserID)
	}
	return nil
}
