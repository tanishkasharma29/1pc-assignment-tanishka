package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/lemonn/typecode-middleware/internal/service"
)

// Handler serves the proxied resources.
//
// The handlers are deliberately thin. They are invoked only on a cache miss, so
// keeping them free of caching concerns is what lets the caching middleware stay
// generic: it wraps these exactly as it would wrap any other handler.
type Handler struct {
	svc *service.Service
	log *slog.Logger
}

func NewHandler(svc *service.Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

// GetPost serves GET /posts/{id}. The id was validated upstream of here by
// requireResourceID, so it is known to be a positive integer within bounds.
func (h *Handler) GetPost(w http.ResponseWriter, r *http.Request) {
	post, err := h.svc.Post(r.Context(), resourceIDFrom(r.Context()))
	if err != nil {
		writeError(w, r, err, h.log)
		return
	}
	writeJSON(w, r, http.StatusOK, post, h.log)
}

// ListPosts serves GET /posts. The query has already been sanitised, so
// r.URL.Query() contains only supported, validated parameters and can be
// forwarded as-is.
func (h *Handler) ListPosts(w http.ResponseWriter, r *http.Request) {
	posts, err := h.svc.Posts(r.Context(), r.URL.Query())
	if err != nil {
		writeError(w, r, err, h.log)
		return
	}
	writeJSON(w, r, http.StatusOK, posts, h.log)
}

// GetTodo serves GET /todos/{id}.
func (h *Handler) GetTodo(w http.ResponseWriter, r *http.Request) {
	todo, err := h.svc.Todo(r.Context(), resourceIDFrom(r.Context()))
	if err != nil {
		writeError(w, r, err, h.log)
		return
	}
	writeJSON(w, r, http.StatusOK, todo, h.log)
}

// ListTodos serves GET /todos.
func (h *Handler) ListTodos(w http.ResponseWriter, r *http.Request) {
	todos, err := h.svc.Todos(r.Context(), r.URL.Query())
	if err != nil {
		writeError(w, r, err, h.log)
		return
	}
	writeJSON(w, r, http.StatusOK, todos, h.log)
}
