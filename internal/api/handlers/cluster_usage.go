package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	clusterUsageWindow = 7 * 24 * time.Hour
	clusterUsageStep   = time.Hour
	promCPUUsageQuery  = `sum(vmware_host_cpu_usage)`
	promCPUMaxQuery    = `sum(vmware_host_cpu_max)`
	promRAMUsageQuery  = `sum(vmware_host_memory_usage)`
	promRAMMaxQuery    = `sum(vmware_host_memory_max)`
)

// Deprecated PromQL kept only for test name stability in older comments;
// absolute series are preferred. Percent is derived from absolute gauges.
const (
	promCPUQuery = `100 * sum(vmware_host_cpu_usage) / clamp_min(sum(vmware_host_cpu_max), 1)`
	promRAMQuery = `100 * sum(vmware_host_memory_usage) / clamp_min(sum(vmware_host_memory_max), 1)`
)

type clusterUsagePoint struct {
	T float64 `json:"t"`
	V float64 `json:"v"`
}

type clusterUsageResponse struct {
	Available        bool               `json:"available"`
	CPUPercent       *float64           `json:"cpu_percent"`
	RAMPercent       *float64           `json:"ram_percent"`
	HostCPUUsageMHz  *float64           `json:"host_cpu_usage_mhz"`
	HostCPUMaxMHz    *float64           `json:"host_cpu_max_mhz"`
	HostRAMUsageMB   *float64           `json:"host_ram_usage_mb"`
	HostRAMMaxMB     *float64           `json:"host_ram_max_mb"`
	Series           clusterUsageSeries `json:"series"`
	AllocatedVCPUs   int                `json:"allocated_vcpus"`
	AllocatedRAMMB   int                `json:"allocated_ram_mb"`
	ActivePods       int                `json:"active_pods"`
	Message          string             `json:"message,omitempty"`
}

type clusterUsageSeries struct {
	CPU        []clusterUsagePoint `json:"cpu"`
	RAM        []clusterUsagePoint `json:"ram"`
	CPUUsageMHz []clusterUsagePoint `json:"cpu_usage_mhz"`
	RAMUsageMB  []clusterUsagePoint `json:"ram_usage_mb"`
}

type promAPIResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
	Error string `json:"error"`
}

type promInstantResult []struct {
	Value [2]any `json:"value"`
}

type promRangeResult []struct {
	Values [][2]any `json:"values"`
}

// AdminClusterUsage returns SQL allocation gauges plus optional 7-day host
// CPU/RAM history from Prometheus. Absolute host totals (MHz / MB) are
// included so the instructor overview can show used/max, not only percent.
// The client query string is ignored; PromQL is fixed in this handler. Empty
// or failed Prometheus still returns 200 with available=false so the UI can
// keep the SQL gauges.
func (h *Handler) AdminClusterUsage(w http.ResponseWriter, r *http.Request) {
	out := clusterUsageResponse{
		Series: clusterUsageSeries{
			CPU:         []clusterUsagePoint{},
			RAM:         []clusterUsagePoint{},
			CPUUsageMHz: []clusterUsagePoint{},
			RAMUsageMB:  []clusterUsagePoint{},
		},
	}
	if h.db != nil {
		alloc, err := h.db.GetClusterAllocatedUsage(r.Context())
		if err != nil {
			h.logger.Error("cluster allocated usage failed", "error", err)
		} else {
			out.AllocatedVCPUs = alloc.VCPUs
			out.AllocatedRAMMB = alloc.RAMMB
			out.ActivePods = alloc.Pods
		}
	}

	if h.prometheusURL == "" || h.promHTTP == nil {
		out.Message = "Capacity history unavailable"
		respondJSON(w, http.StatusOK, out)
		return
	}

	now := time.Now()
	start := now.Add(-clusterUsageWindow)
	cpuUsage, errCPUUsage := h.promInstant(r, promCPUUsageQuery)
	cpuMax, errCPUMax := h.promInstant(r, promCPUMaxQuery)
	ramUsage, errRAMUsage := h.promInstant(r, promRAMUsageQuery)
	ramMax, errRAMMax := h.promInstant(r, promRAMMaxQuery)
	cpuSeriesAbs, errCPURange := h.promRange(r, promCPUUsageQuery, start, now)
	ramSeriesAbs, errRAMRange := h.promRange(r, promRAMUsageQuery, start, now)
	if errCPUUsage != nil || errCPUMax != nil || errRAMUsage != nil || errRAMMax != nil ||
		errCPURange != nil || errRAMRange != nil {
		h.logger.Error("prometheus cluster usage failed",
			"cpu_usage", errCPUUsage, "cpu_max", errCPUMax,
			"ram_usage", errRAMUsage, "ram_max", errRAMMax,
			"cpu_range", errCPURange, "ram_range", errRAMRange)
		out.Message = "Capacity history unavailable"
		respondJSON(w, http.StatusOK, out)
		return
	}
	if len(cpuSeriesAbs) == 0 && len(ramSeriesAbs) == 0 &&
		cpuUsage == nil && cpuMax == nil && ramUsage == nil && ramMax == nil {
		out.Message = "Capacity history unavailable"
		respondJSON(w, http.StatusOK, out)
		return
	}

	out.Available = true
	out.HostCPUUsageMHz = cpuUsage
	out.HostCPUMaxMHz = cpuMax
	out.HostRAMUsageMB = ramUsage
	out.HostRAMMaxMB = ramMax
	out.CPUPercent = percentOf(cpuUsage, cpuMax)
	out.RAMPercent = percentOf(ramUsage, ramMax)
	out.Series.CPUUsageMHz = cpuSeriesAbs
	out.Series.RAMUsageMB = ramSeriesAbs
	out.Series.CPU = percentSeries(cpuSeriesAbs, cpuMax)
	out.Series.RAM = percentSeries(ramSeriesAbs, ramMax)
	respondJSON(w, http.StatusOK, out)
}

func percentOf(usage, max *float64) *float64 {
	if usage == nil || max == nil || *max <= 0 {
		return nil
	}
	v := 100 * (*usage) / (*max)
	return &v
}

func percentSeries(abs []clusterUsagePoint, max *float64) []clusterUsagePoint {
	out := make([]clusterUsagePoint, 0, len(abs))
	if max == nil || *max <= 0 {
		return out
	}
	for _, p := range abs {
		out = append(out, clusterUsagePoint{T: p.T, V: 100 * p.V / *max})
	}
	return out
}

func (h *Handler) promInstant(r *http.Request, query string) (*float64, error) {
	u := h.prometheusURL + "/api/v1/query?query=" + url.QueryEscape(query)
	raw, err := h.promGet(r, u)
	if err != nil {
		return nil, err
	}
	var results promInstantResult
	if err := json.Unmarshal(raw, &results); err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, nil
	}
	v, err := promSampleValue(results[0].Value[1])
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (h *Handler) promRange(r *http.Request, query string, start, end time.Time) ([]clusterUsagePoint, error) {
	q := url.Values{}
	q.Set("query", query)
	q.Set("start", strconv.FormatInt(start.Unix(), 10))
	q.Set("end", strconv.FormatInt(end.Unix(), 10))
	q.Set("step", strconv.FormatInt(int64(clusterUsageStep.Seconds()), 10))
	u := h.prometheusURL + "/api/v1/query_range?" + q.Encode()
	raw, err := h.promGet(r, u)
	if err != nil {
		return nil, err
	}
	var results promRangeResult
	if err := json.Unmarshal(raw, &results); err != nil {
		return nil, err
	}
	points := make([]clusterUsagePoint, 0)
	if len(results) == 0 {
		return points, nil
	}
	for _, sample := range results[0].Values {
		t, err := promSampleTimestamp(sample[0])
		if err != nil {
			return nil, err
		}
		v, err := promSampleValue(sample[1])
		if err != nil {
			return nil, err
		}
		points = append(points, clusterUsagePoint{T: t, V: v})
	}
	return points, nil
}

func (h *Handler) promGet(r *http.Request, rawURL string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := h.promHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus status %d", resp.StatusCode)
	}
	var parsed promAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	if parsed.Status != "success" {
		if parsed.Error != "" {
			return nil, fmt.Errorf("prometheus: %s", parsed.Error)
		}
		return nil, fmt.Errorf("prometheus status %q", parsed.Status)
	}
	return parsed.Data.Result, nil
}

func promSampleTimestamp(v any) (float64, error) {
	switch t := v.(type) {
	case float64:
		return t, nil
	case json.Number:
		return t.Float64()
	case string:
		return strconv.ParseFloat(t, 64)
	default:
		return 0, fmt.Errorf("unexpected timestamp %T", v)
	}
}

func promSampleValue(v any) (float64, error) {
	switch t := v.(type) {
	case string:
		return strconv.ParseFloat(t, 64)
	case float64:
		return t, nil
	case json.Number:
		return t.Float64()
	default:
		return 0, fmt.Errorf("unexpected sample %T", v)
	}
}
