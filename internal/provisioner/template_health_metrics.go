// template_health_metrics.go — Pushgateway pusher for template health checks.
//
// Metrics family:
//
//	crucible_template_health_checker_up          — gauge: 1 when last cycle
//	                                               completed without a
//	                                               vCenter-level failure, 0 when
//	                                               vCenter was unreachable/auth-
//	                                               expired. Fires CrucibleTemplate-
//	                                               HealthCheckerDown instead of N
//	                                               per-template alerts during an
//	                                               outage.
//
//	crucible_template_health_status{template,     — gauge: 1=healthy, 0=unhealthy.
//	            check_type}                        check_type is "structural" or
//	                                               "deep". Only emitted after the
//	                                               first check of each type.
//
//	crucible_template_health_last_check_timestamp — gauge: Unix seconds of the
//	_seconds{template}                             most recent check (any type).
//	                                               Stale when now()-value > 30h
//	                                               (CrucibleTemplateHealthChecker-
//	                                               Stale alert).
//
//	crucible_template_health_check_duration_      — summary (sum+count) per
//	seconds_{sum,count}{check_type}                check_type. Average latency =
//	                                               rate(sum)/rate(count).
package provisioner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// TemplateHealthPusher accumulates template health metrics and pushes them to
// Pushgateway on demand. Zero value is usable but inert (BaseURL empty ⇒
// Push is a no-op). Construct via NewTemplateHealthPusher.
type TemplateHealthPusher struct {
	BaseURL        string
	Job            string
	GroupingLabels map[string]string

	// HTTP overrides the client; nil falls back to a 10s-timeout client.
	HTTP *http.Client

	mu sync.Mutex

	// 1 when the last cycle ran without a vCenter-level failure, 0 when it
	// didn't. Omitted until SetCheckerUp has been called once.
	checkerUp          float64
	checkerUpCollected bool

	// Per-template-and-check-type health gauges (1=healthy, 0=unhealthy).
	// Key: "templateName|checkType".
	healthStatus map[string]float64

	// Per-template last-check timestamps (Unix seconds).
	lastCheckTimestamp map[string]float64

	// Per-check-type duration counters (summary without quantiles).
	// Key: checkType.
	durationSum   map[string]float64
	durationCount map[string]float64
}

// NewTemplateHealthPusher returns an initialised pusher. baseURL may be empty,
// in which case Push() is a no-op but all Set*/Record* calls remain safe.
func NewTemplateHealthPusher(baseURL, job string, grouping map[string]string) *TemplateHealthPusher {
	if job == "" {
		job = "crucible_provision_worker"
	}
	return &TemplateHealthPusher{
		BaseURL:            baseURL,
		Job:                job,
		GroupingLabels:     grouping,
		healthStatus:       map[string]float64{},
		lastCheckTimestamp: map[string]float64{},
		durationSum:        map[string]float64{},
		durationCount:      map[string]float64{},
	}
}

// SetCheckerUp records whether the last cycle completed without a
// vCenter-level infrastructure failure (1 = up, 0 = down).
func (p *TemplateHealthPusher) SetCheckerUp(up float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.checkerUp = up
	p.checkerUpCollected = true
}

// SetHealthStatus records the per-template, per-check-type health gauge.
// healthy is 1 for healthy, 0 for unhealthy.
func (p *TemplateHealthPusher) SetHealthStatus(templateName, checkType string, healthy float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.healthStatus[templateName+"|"+checkType] = healthy
}

// SetLastCheckTimestamp records the Unix timestamp of the most recent check
// for a template (used by the staleness alert).
func (p *TemplateHealthPusher) SetLastCheckTimestamp(templateName string, unixSec float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastCheckTimestamp[templateName] = unixSec
}

// RecordStructuralResult records a completed structural check's duration.
func (p *TemplateHealthPusher) RecordStructuralResult(templateName string, durationSec float64, _ bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.durationSum["structural"] += durationSec
	p.durationCount["structural"]++
}

// RecordDeepResult records a completed deep check's duration.
func (p *TemplateHealthPusher) RecordDeepResult(templateName string, durationSec float64, _ bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.durationSum["deep"] += durationSec
	p.durationCount["deep"]++
}

// Push serialises the current values and POSTs them to Pushgateway. No-op
// when BaseURL is empty.
func (p *TemplateHealthPusher) Push(ctx context.Context) error {
	if p.BaseURL == "" {
		return nil
	}
	body := p.serialize()
	target := destroyFailedPushURL(p.BaseURL, p.Job,
		mergeGrouping(p.GroupingLabels, "component", "template_health"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build push request: %w", err)
	}
	req.Header.Set("Content-Type", "text/plain; version=0.0.4")
	client := p.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("push: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("pushgateway %d: %s", resp.StatusCode, string(buf))
	}
	return nil
}

func (p *TemplateHealthPusher) serialize() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()

	var b bytes.Buffer

	// crucible_template_health_checker_up --------------------------------
	// Only emit after the first SetCheckerUp call so an absent series is
	// honest ("checker has never run") rather than looking like "checker is up".
	if p.checkerUpCollected {
		fmt.Fprintf(&b, "# HELP crucible_template_health_checker_up 1 when the template health checker last completed without a vCenter-infrastructure failure; 0 when vCenter was unreachable or auth-expired. Use this to fire a single CrucibleTemplateHealthCheckerDown alert instead of N per-template alerts during an outage.\n")
		fmt.Fprintf(&b, "# TYPE crucible_template_health_checker_up gauge\n")
		fmt.Fprintf(&b, "crucible_template_health_checker_up %g\n", p.checkerUp)
	}

	// crucible_template_health_status{template, check_type} --------------
	if len(p.healthStatus) > 0 {
		fmt.Fprintf(&b, "# HELP crucible_template_health_status 1 when the last check of check_type for this template passed, 0 when it failed.\n")
		fmt.Fprintf(&b, "# TYPE crucible_template_health_status gauge\n")
		keys := sortedKeys(p.healthStatus)
		for _, k := range keys {
			parts := strings.SplitN(k, "|", 2)
			template, checkType := "", k
			if len(parts) == 2 {
				template, checkType = parts[0], parts[1]
			}
			fmt.Fprintf(&b, "crucible_template_health_status{template=%q,check_type=%q} %g\n",
				template, checkType, p.healthStatus[k])
		}
	}

	// crucible_template_health_last_check_timestamp_seconds{template} ----
	if len(p.lastCheckTimestamp) > 0 {
		fmt.Fprintf(&b, "# HELP crucible_template_health_last_check_timestamp_seconds Unix timestamp of the most recent health check (any type) per template. Alert when now()-value exceeds 30h (2.5 missed 12h cycles).\n")
		fmt.Fprintf(&b, "# TYPE crucible_template_health_last_check_timestamp_seconds gauge\n")
		keys := sortedKeys(p.lastCheckTimestamp)
		for _, k := range keys {
			fmt.Fprintf(&b, "crucible_template_health_last_check_timestamp_seconds{template=%q} %g\n",
				k, p.lastCheckTimestamp[k])
		}
	}

	// crucible_template_health_check_duration_seconds_{sum,count} --------
	if len(p.durationSum) > 0 {
		fmt.Fprintf(&b, "# HELP crucible_template_health_check_duration_seconds_sum Cumulative wall-clock seconds per check type. Average = rate(sum)/rate(count).\n")
		fmt.Fprintf(&b, "# TYPE crucible_template_health_check_duration_seconds_sum counter\n")
		keys := sortedKeys(p.durationSum)
		for _, k := range keys {
			fmt.Fprintf(&b, "crucible_template_health_check_duration_seconds_sum{check_type=%q} %g\n", k, p.durationSum[k])
		}

		fmt.Fprintf(&b, "# HELP crucible_template_health_check_duration_seconds_count Number of completed checks per type. Average = rate(sum)/rate(count).\n")
		fmt.Fprintf(&b, "# TYPE crucible_template_health_check_duration_seconds_count counter\n")
		for _, k := range keys {
			fmt.Fprintf(&b, "crucible_template_health_check_duration_seconds_count{check_type=%q} %g\n", k, p.durationCount[k])
		}
	}

	fmt.Fprintf(&b, "# HELP crucible_template_health_run_timestamp_seconds Unix time of the latest template health metrics push.\n")
	fmt.Fprintf(&b, "# TYPE crucible_template_health_run_timestamp_seconds gauge\n")
	fmt.Fprintf(&b, "crucible_template_health_run_timestamp_seconds %d\n", time.Now().Unix())

	return b.Bytes()
}

// sortedKeys returns the map keys in sorted order for deterministic output.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
