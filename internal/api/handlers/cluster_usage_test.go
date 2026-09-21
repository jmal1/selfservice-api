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
	if got.Series.CPUUsageMHz == nil || got.Series.RAMUsageMB == nil {
		t.Fatal("absolute series slices must be empty arrays, not null")
	}
}

func TestAdminClusterUsage_IgnoresClientQuery(t *testing.T) {
	var seen []string
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		seen = append(seen, q)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "query_range") {
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"values":[[1,"1000"],[2,"2000"]]}]}}`))
			return
		}
		switch q {
		case promCPUUsageQuery:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"value":[1,"1250"]}]}}`))
		case promCPUMaxQuery:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"value":[1,"10000"]}]}}`))
		case promRAMUsageQuery:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"value":[1,"8192"]}]}}`))
		case promRAMMaxQuery:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"value":[1,"65536"]}]}}`))
		default:
			t.Errorf("unexpected PromQL %q", q)
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		}
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
	if got.HostCPUUsageMHz == nil || *got.HostCPUUsageMHz != 1250 {
		t.Fatalf("host_cpu_usage_mhz = %v, want 1250", got.HostCPUUsageMHz)
	}
	if got.HostCPUMaxMHz == nil || *got.HostCPUMaxMHz != 10000 {
		t.Fatalf("host_cpu_max_mhz = %v, want 10000", got.HostCPUMaxMHz)
	}
	if got.CPUPercent == nil || *got.CPUPercent != 12.5 {
		t.Fatalf("cpu_percent = %v, want 12.5", got.CPUPercent)
	}
	if got.HostRAMUsageMB == nil || *got.HostRAMUsageMB != 8192 {
		t.Fatalf("host_ram_usage_mb = %v, want 8192", got.HostRAMUsageMB)
	}
	if got.HostRAMMaxMB == nil || *got.HostRAMMaxMB != 65536 {
		t.Fatalf("host_ram_max_mb = %v, want 65536", got.HostRAMMaxMB)
	}
	if len(got.Series.CPUUsageMHz) != 2 || got.Series.CPUUsageMHz[1].V != 2000 {
		t.Fatalf("cpu abs series = %+v", got.Series.CPUUsageMHz)
	}
	if len(got.Series.CPU) != 2 || got.Series.CPU[1].V != 20 {
		t.Fatalf("cpu percent series = %+v", got.Series.CPU)
	}
	for _, q := range seen {
		if q == "up" {
			t.Fatalf("forwarded client query %q", q)
		}
	}
	if len(seen) != 6 {
		t.Fatalf("prometheus calls = %d, want 6 (4 instant + 2 range)", len(seen))
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
