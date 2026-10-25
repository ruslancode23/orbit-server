package linkpreview

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Handler serves cached preview images through this server. It is mounted beside
// the generated routes: the browser loads it as an <img>, which cannot carry the
// session header, and the response is binary.
type Handler struct {
	svc *Service
	log *slog.Logger
}

// NewHandler returns the image proxy.
func NewHandler(svc *Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

// Image answers GET /api/v1/link-previews/{previewId}/image.
func (h *Handler) Image(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "previewId"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	image, contentType, err := h.svc.Image(r.Context(), id)
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrBlocked), errors.Is(err, ErrNotHTML):
		http.NotFound(w, r)
		return
	case errors.Is(err, ErrTooLarge):
		http.Error(w, "the image is too large", http.StatusRequestEntityTooLarge)
		return
	case err != nil:
		h.log.ErrorContext(r.Context(), "preview image", slog.Any("error", err))
		http.Error(w, "could not load the image", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", contentType)
	// The id names a cached card: the same image for a while is fine.
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	//nolint:gosec // Image approved the content type: png, jpeg, webp or gif.
	_, _ = w.Write(image)
}
