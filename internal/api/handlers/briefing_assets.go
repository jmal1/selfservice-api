package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/jmal1/selfservice-api/internal/audit"
	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/middleware"
)

// BriefingAssetMaxBytes matches the CHECK on briefing_assets.size_bytes.
const BriefingAssetMaxBytes = 5 << 20

// BriefingAssetStore is the object-store surface for lab briefing images.
// Satisfied structurally by *objectstore.Client.
type BriefingAssetStore interface {
	Put(ctx context.Context, key string, data io.Reader, size int64, contentType string) error
	Open(ctx context.Context, key string) (io.ReadCloser, int64, error)
}

// WithBriefingAssetStore wires image storage for lab briefings. Without it
// both briefing-asset routes answer 503.
func (h *Handler) WithBriefingAssetStore(store BriefingAssetStore) *Handler {
	h.briefingAssets = store
	return h
}

// Raster formats only: SVG can carry script, so it is never accepted.
var briefingImageTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
}

var briefingAssetSHA = regexp.MustCompile(`^[0-9a-f]{64}$`)

func briefingAssetKey(sha string) string { return "crucible/briefings/" + sha }

// AdminUploadBriefingAsset stores one image (raw request body). The type is
// sniffed from the bytes, never taken from the request headers.
func (h *Handler) AdminUploadBriefingAsset(w http.ResponseWriter, r *http.Request) {
	if h.briefingAssets == nil {
		respondError(w, r, http.StatusServiceUnavailable, "briefing image upload is not configured")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, BriefingAssetMaxBytes))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		respondError(w, r, http.StatusRequestEntityTooLarge, "images are limited to 5 MiB")
		return
	case err != nil:
		respondError(w, r, http.StatusBadRequest, "could not read the image")
		return
	case len(body) == 0:
		respondError(w, r, http.StatusBadRequest, "empty upload")
		return
	}
	contentType, _, _ := mime.ParseMediaType(http.DetectContentType(body))
	if !briefingImageTypes[contentType] {
		respondError(w, r, http.StatusUnsupportedMediaType, "only PNG, JPEG, GIF or WebP images are allowed (SVG is not)")
		return
	}
	sum := sha256.Sum256(body)
	asset := database.BriefingAsset{SHA256: hex.EncodeToString(sum[:]), ContentType: contentType, SizeBytes: int64(len(body))}

	existing, err := h.db.GetBriefingAsset(r.Context(), asset.SHA256)
	if err != nil {
		h.logger.Error("briefing asset lookup failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if existing == nil {
		// Object first, then the row: a row always means the object exists.
		if err := h.briefingAssets.Put(r.Context(), briefingAssetKey(asset.SHA256), bytes.NewReader(body), asset.SizeBytes, contentType); err != nil {
			h.logger.Error("briefing asset put failed", "error", err)
			respondError(w, r, http.StatusBadGateway, "could not store the image")
			return
		}
		if err := h.db.RecordBriefingAsset(r.Context(), asset, middleware.UserIDFromContext(r.Context())); err != nil {
			h.logger.Error("briefing asset record failed", "error", err)
			respondError(w, r, http.StatusInternalServerError, "internal error")
			return
		}
	}
	audit.Log(r.Context(), h.db, "briefing.asset_upload",
		audit.FromRequest(r),
		audit.Detail("sha256", asset.SHA256),
		audit.Detail("size_bytes", asset.SizeBytes),
	)
	respondJSON(w, http.StatusCreated, map[string]any{
		"sha256":       asset.SHA256,
		"url":          "/api/v1/briefing-assets/" + asset.SHA256,
		"content_type": asset.ContentType,
		"size_bytes":   asset.SizeBytes,
	})
}

// GetBriefingAsset serves an image to any signed-in user. Content-Type comes
// from the stored row, sniffing is disabled, and the response is sandboxed
// so a direct navigation can never execute anything.
func (h *Handler) GetBriefingAsset(w http.ResponseWriter, r *http.Request) {
	sha := chi.URLParam(r, "sha256")
	if !briefingAssetSHA.MatchString(sha) {
		respondError(w, r, http.StatusNotFound, "not found")
		return
	}
	if h.briefingAssets == nil {
		respondError(w, r, http.StatusServiceUnavailable, "briefing images are not configured")
		return
	}
	asset, err := h.db.GetBriefingAsset(r.Context(), sha)
	if err != nil {
		h.logger.Error("briefing asset lookup failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if asset == nil {
		respondError(w, r, http.StatusNotFound, "not found")
		return
	}
	rc, size, err := h.briefingAssets.Open(r.Context(), briefingAssetKey(sha))
	if err != nil {
		h.logger.Error("briefing asset open failed", "error", err, "sha256", sha)
		respondError(w, r, http.StatusBadGateway, "could not read the image")
		return
	}
	defer rc.Close()
	hdr := w.Header()
	hdr.Set("Content-Type", asset.ContentType)
	hdr.Set("Content-Length", strconv.FormatInt(size, 10))
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	hdr.Set("Cache-Control", "private, max-age=31536000, immutable")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}
