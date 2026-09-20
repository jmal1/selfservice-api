package handlers

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminClusterUsage_EmptyPrometheusStillOK(t *testing.T) {
	h := &Handler{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/cluster-usage?query=up", nil)
	rec := httptest.NewRecorder()
	h.AdminClusterUsage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got clusterUsageResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Available {
		t.Fatal("available = true, want false when Prometheus is unset")
	}
	if got.Series.CPU == nil || got.Series.RAM == nil {
		t.Fatal("series slices must be empty arrays, not null")
	}
}

func TestAdminClusterUsage_IgnoresClientQuery(t *testing.T) {
	var seen []string
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Query().Get("query"))
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "query_range") {
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"values":[[1,"10"],[2,"20"]]}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"value":[1,"12.5"]}]}}`))
	}))
	defer prom.Close()

	h := &Handler{
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		prometheusURL: prom.URL,
		promHTTP:      prom.Client(),
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/cluster-usage?query=up", nil)
	rec := httptest.NewRecorder()
	h.AdminClusterUsage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}
	var got clusterUsageResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Available {
		t.Fatal("available = false, want true")
	}
	if got.CPUPercent == nil || *got.CPUPercent != 12.5 {
		t.Fatalf("cpu_percent = %v, want 12.5", got.CPUPercent)
	}
	if len(got.Series.CPU) != 2 || got.Series.CPU[1].V != 20 {
		t.Fatalf("cpu series = %+v", got.Series.CPU)
	}
	for _, q := range seen {
		if q == "up" {
			t.Fatalf("forwarded client query %q", q)
		}
		if q != promCPUQuery && q != promRAMQuery {
			t.Fatalf("unexpected PromQL %q", q)
		}
	}
	if len(seen) != 4 {
		t.Fatalf("prometheus calls = %d, want 4 (2 instant + 2 range)", len(seen))
	}
}

func TestAdminClusterUsage_PrometheusFailureHidesGraph(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	defer prom.Close()

	h := &Handler{
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		prometheusURL: prom.URL,
		promHTTP:      prom.Client(),
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/cluster-usage?query=up", nil)
	rec := httptest.NewRecorder()
	h.AdminClusterUsage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got clusterUsageResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Available {
		t.Fatal("available = true after Prometheus failure")
	}
}
