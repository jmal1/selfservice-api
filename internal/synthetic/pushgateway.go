package synthetic

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Pushgateway pushes synthetic check results in Prometheus text exposition
// format to a Prometheus Pushgateway instance.
//
// Job naming convention: the job label identifies the synthetic runner
// itself (e.g. "crucible_synthetic_api"). Per-check identity is encoded via
// the `check` label on each metric. This lets a single push call replace ALL
// metrics for the entire run atomically — important because Pushgateway only
// expires metrics on explicit DELETE.
type Pushgateway struct {
	// BaseURL is the Pushgateway root, e.g. "http://pushgateway:9091".
	BaseURL string

	// Job is the value of the {job} label segment in the push URL. Pushgateway
	// requires this exists and replaces metrics with the same job+grouping.
	Job string

	// GroupingLabels are appended to the push URL after /metrics/job/{job}.
	// Each kv pair becomes /{key}/{value}. Use this for the `layer` label
	// (api vs ui) so internal and external suites do not clobber each other.
	GroupingLabels map[string]string

	// HTTP is the transport; nil falls back to http.DefaultClient with a 10s
	// timeout. Tests inject httptest.NewServer().Client().
	HTTP *http.Client
}

// NewPushgateway constructs a Pushgateway client with a fixed job name and
// optional grouping labels. baseURL MUST NOT contain a trailing slash.
func NewPushgateway(baseURL, job string, grouping map[string]string) *Pushgateway {
	return &Pushgateway{
		BaseURL:        strings.TrimRight(baseURL, "/"),
		Job:            job,
		GroupingLabels: grouping,
		HTTP:           &http.Client{Timeout: 10 * time.Second},
	}
}

// pushURL builds the Pushgateway path for this job + grouping. We sort
// grouping keys so the URL is deterministic — Pushgateway treats different
// orderings as different groupings, which would leak old metrics forever.
func (p *Pushgateway) pushURL() string {
	var b strings.Builder
	b.WriteString(p.BaseURL)
	b.WriteString("/metrics/job/")
	b.WriteString(url.PathEscape(p.Job))
	keys := make([]string, 0, len(p.GroupingLabels))
	for k := range p.GroupingLabels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString("/")
		b.WriteString(url.PathEscape(k))
		b.WriteString("/")
		b.WriteString(url.PathEscape(p.GroupingLabels[k]))
	}
	return b.String()
}

// PushResults serializes the given results into Prometheus text format and
// POSTs them to Pushgateway. It returns an error on any non-2xx response so
// the caller can log it but does NOT panic — synthetic monitoring must
// degrade gracefully (silent metrics is worse than the run itself failing).
//
// The exposition format includes both HELP and TYPE for each metric family —
// without TYPE, Pushgateway >=1.0 rejects the push with 400.
func (p *Pushgateway) PushResults(ctx context.Context, results []Result) error {
	body := serializeResults(results)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.pushURL(), bytes.NewReader(body))
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
		return fmt.Errorf("pushgateway returned %d: %s", resp.StatusCode, string(buf))
	}
	return nil
}

// serializeResults builds the Prometheus text-format body for a result batch.
// It is exported only via the package-internal tests to make assertions
// possible without spinning up an HTTP server.
//
// Title and Runbook are emitted as labels on every per-check metric so
// dashboards and alert templates can render the friendly name and operator
// guidance without a join. Description is verbose; it lives only on the
// dedicated `crucible_synthetic_check_info` metric so per-cycle time-series
// stay slim.
func serializeResults(results []Result) []byte {
	var b bytes.Buffer
	// Metric: crucible_synthetic_check_success — gauge 0 or 1.
	b.WriteString("# HELP crucible_synthetic_check_success 1 if the check passed, 0 otherwise.\n")
	b.WriteString("# TYPE crucible_synthetic_check_success gauge\n")
	for _, r := range results {
		fmt.Fprintf(&b, `crucible_synthetic_check_success{check=%q,title=%q,runbook=%q,severity=%q} %d`+"\n",
			r.Name, sanitizeLabel(r.Title), sanitizeLabel(r.Runbook), string(r.Severity), boolToInt(r.Success))
	}
	// Metric: crucible_synthetic_check_duration_seconds — gauge of last run.
	b.WriteString("# HELP crucible_synthetic_check_duration_seconds Wall-clock duration of the last run.\n")
	b.WriteString("# TYPE crucible_synthetic_check_duration_seconds gauge\n")
	for _, r := range results {
		fmt.Fprintf(&b, `crucible_synthetic_check_duration_seconds{check=%q,title=%q,runbook=%q,severity=%q} %f`+"\n",
			r.Name, sanitizeLabel(r.Title), sanitizeLabel(r.Runbook), string(r.Severity), r.Duration.Seconds())
	}
	// Metric: crucible_synthetic_check_http_status — gauge of last HTTP code,
	// 0 if no HTTP call completed (e.g. DNS failure before send).
	b.WriteString("# HELP crucible_synthetic_check_http_status HTTP status of the last response, 0 if no response.\n")
	b.WriteString("# TYPE crucible_synthetic_check_http_status gauge\n")
	for _, r := range results {
		fmt.Fprintf(&b, `crucible_synthetic_check_http_status{check=%q,title=%q,runbook=%q,severity=%q} %d`+"\n",
			r.Name, sanitizeLabel(r.Title), sanitizeLabel(r.Runbook), string(r.Severity), r.HTTPStatus)
	}
	// Metric: crucible_synthetic_check_attempts — gauge of how many attempts
	// the runner needed. 1 means first-try pass or first-try fail with no
	// retry policy. Values >1 mean a retry was applied. A check that
	// consistently shows attempts>1 is a leading indicator of infrastructure
	// degradation even while crucible_synthetic_check_success stays 1.
	b.WriteString("# HELP crucible_synthetic_check_attempts Number of attempts the runner made for this check in the last cycle (1=first-try, 2=one retry, etc).\n")
	b.WriteString("# TYPE crucible_synthetic_check_attempts gauge\n")
	for _, r := range results {
		attempts := r.Attempts
		if attempts < 1 {
			attempts = 1 // defensive: Attempts=0 means the field was not set
		}
		fmt.Fprintf(&b, `crucible_synthetic_check_attempts{check=%q,title=%q,runbook=%q,severity=%q} %d`+"\n",
			r.Name, sanitizeLabel(r.Title), sanitizeLabel(r.Runbook), string(r.Severity), attempts)
	}
	// Metric: crucible_synthetic_vcenter_degraded — 1 when at least one
	// attempt in this cycle failed with a vCenter-attributable error. The
	// check may still be green (Success=true) if a retry succeeded; this
	// gauge stays 1 to surface vCenter wobble even when provisioning
	// ultimately completed. Use this for a leading-indicator alert that fires
	// before checks start fully failing.
	b.WriteString("# HELP crucible_synthetic_vcenter_degraded 1 if any attempt in the last cycle was attributed to vCenter slowness or unreachability.\n")
	b.WriteString("# TYPE crucible_synthetic_vcenter_degraded gauge\n")
	for _, r := range results {
		fmt.Fprintf(&b, `crucible_synthetic_vcenter_degraded{check=%q,title=%q,runbook=%q,severity=%q} %d`+"\n",
			r.Name, sanitizeLabel(r.Title), sanitizeLabel(r.Runbook), string(r.Severity), boolToInt(r.VCenterDegraded))
	}
	// Metric: crucible_synthetic_check_info — constant 1 carrying friendly
	// metadata as labels. Standard `_info`-metric pattern: dashboards join
	// against this on the `check` label to surface title/description.
	b.WriteString("# HELP crucible_synthetic_check_info Static metadata about each synthetic check.\n")
	b.WriteString("# TYPE crucible_synthetic_check_info gauge\n")
	for _, r := range results {
		fmt.Fprintf(&b, `crucible_synthetic_check_info{check=%q,title=%q,description=%q,runbook=%q,severity=%q} 1`+"\n",
			r.Name, sanitizeLabel(r.Title), sanitizeLabel(r.Description), sanitizeLabel(r.Runbook), string(r.Severity))
	}
	// Metric: crucible_synthetic_checks_registered — how many checks this cycle
	// actually ran.
	//
	// This is the generic guard for a failure mode that is otherwise invisible.
	// PushResults POSTs each metric family, and Pushgateway REPLACES a family
	// wholesale on POST, so a check that stops being registered does not go
	// stale — its series ceases to exist. Every alert we have is shaped like
	// `1 - crucible_synthetic_check_success > 0`, which cannot match an absent
	// series, so silently dropping a check reads as a perfectly green board.
	//
	// Per-feature meta-checks (see checks.ElevatedIdentityConfigured) cover one
	// case each and have to be remembered every time. This counter covers ALL of
	// them at once, and alerting on a DECREASE rather than on a fixed threshold
	// means it never needs updating as checks are added.
	b.WriteString("# HELP crucible_synthetic_checks_registered Number of checks executed in this run. A decrease means coverage was silently lost.\n")
	b.WriteString("# TYPE crucible_synthetic_checks_registered gauge\n")
	fmt.Fprintf(&b, "crucible_synthetic_checks_registered %d\n", len(results))
	// Metric: crucible_synthetic_run_timestamp_seconds — unix time of this push.
	// Used by alerts that want to fire if results are stale (no recent push).
	b.WriteString("# HELP crucible_synthetic_run_timestamp_seconds Unix time of the latest synthetic run.\n")
	b.WriteString("# TYPE crucible_synthetic_run_timestamp_seconds gauge\n")
	fmt.Fprintf(&b, "crucible_synthetic_run_timestamp_seconds %d\n", time.Now().Unix())
	return b.Bytes()
}

// sanitizeLabel strips characters that would break Prometheus exposition
// when emitted via %q. Newlines and tabs are mapped to spaces; %q itself
// handles escaping quotes and backslashes, so we only need to flatten
// whitespace. Result is also trimmed to 200 chars to bound label size.
func sanitizeLabel(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		}
		return r
	}, s)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
