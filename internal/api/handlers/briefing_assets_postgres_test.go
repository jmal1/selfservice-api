package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/middleware"
)

type memAssetStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    int
}

func (m *memAssetStore) Put(_ context.Context, key string, data io.Reader, _ int64, _ string) error {
	b, err := io.ReadAll(data)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = b
	m.puts++
	return nil
}

func (m *memAssetStore) Open(_ context.Context, key string) (io.ReadCloser, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[key]
	if !ok {
		return nil, 0, errors.New("missing")
	}
	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}

// A real 1x1 PNG so http.DetectContentType sniffs image/png.
var onePixelPNG, _ = base64.StdEncoding.DecodeString(
	"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==")

func TestBriefingAssetsPostgres(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if testing.Short() || dsn == "" {
		t.Skip("set TEST_DATABASE_URL (and drop -short) to run briefing asset tests")
	}
	if err := database.RunMigrations(dsn); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	instructor := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, oidc_sub, username, email) VALUES ($1, $2, $2, $3)`,
		instructor, instructor.String(), instructor.String()+"@example.invalid"); err != nil {
		t.Fatal(err)
	}

	store := &memAssetStore{objects: map[string][]byte{}}
	h := NewHandler(database.NewQueries(pool), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil).
		WithBriefingAssetStore(store)
	upload := func(body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/briefing-assets", bytes.NewReader(body))
		req.Header.Set("Content-Type", "image/png") // must be ignored: the type is sniffed
		req = req.WithContext(middleware.WithUserID(req.Context(), instructor))
		rec := httptest.NewRecorder()
		h.AdminUploadBriefingAsset(rec, req)
		return rec
	}
	get := func(sha string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/briefing-assets/"+sha, nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("sha256", sha)
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		h.GetBriefingAsset(rec, req)
		return rec
	}

	// A PNG with a random trailer so the hash is unique per test run.
	png := append(append([]byte{}, onePixelPNG...), []byte(uuid.NewString())...)
	sum := sha256.Sum256(png)
	sha := hex.EncodeToString(sum[:])

	rec := upload(png)
	var out struct {
		SHA256      string `json:"sha256"`
		URL         string `json:"url"`
		ContentType string `json:"content_type"`
	}
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &out) != nil ||
		out.SHA256 != sha || out.ContentType != "image/png" || out.URL != "/api/v1/briefing-assets/"+sha {
		t.Fatalf("upload png = %d %s", rec.Code, rec.Body)
	}
	if _, ok := store.objects["crucible/briefings/"+sha]; !ok {
		t.Fatalf("object not stored under crucible/briefings/: %v", store.objects)
	}
	if rec := upload(png); rec.Code != http.StatusCreated || store.puts != 1 {
		t.Fatalf("re-upload must be idempotent without a second put: %d puts=%d", rec.Code, store.puts)
	}

	for name, body := range map[string][]byte{
		"svg":   []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`),
		"html":  []byte(`<html><script>alert(1)</script></html>`),
		"text":  []byte("just text"),
		"empty": nil,
	} {
		rec := upload(body)
		if rec.Code != http.StatusUnsupportedMediaType && rec.Code != http.StatusBadRequest {
			t.Errorf("%s upload = %d, want 415/400", name, rec.Code)
		}
	}
	if rec := upload(append(append([]byte{}, onePixelPNG...), make([]byte, BriefingAssetMaxBytes)...)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized upload = %d, want 413", rec.Code)
	}
	if store.puts != 1 {
		t.Fatalf("rejected uploads must store nothing; puts=%d", store.puts)
	}

	rec = get(sha)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), png) {
		t.Fatalf("get = %d", rec.Code)
	}
	for k, want := range map[string]string{
		"Content-Type":            "image/png",
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "default-src 'none'; sandbox",
	} {
		if got := rec.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
	if rec := get(strings.Repeat("0", 64)); rec.Code != http.StatusNotFound {
		t.Errorf("unknown asset = %d, want 404", rec.Code)
	}
	if rec := get("../../etc/passwd"); rec.Code != http.StatusNotFound {
		t.Errorf("malformed sha = %d, want 404", rec.Code)
	}

	bare := NewHandler(database.NewQueries(pool), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	rec = httptest.NewRecorder()
	bare.AdminUploadBriefingAsset(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(png)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unwired store = %d, want 503", rec.Code)
	}
}
