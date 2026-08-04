package leader

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

// Pusher publishes leader-election metrics to Pushgateway following the same
// hand-serialized text-exposition pattern used by NetworkReconcilePusher,
// IPReconcileCountPusher, and OrphanCountPusher in the provisioner package.
//
// Metrics published:
//
//	crucible_worker_is_leader{pod="<pod>"}               — gauge: 1 if this pod is leader, else 0
//	crucible_worker_leader_transitions_total{pod="<pod>"} — gauge: cumulative leadership state changes
//
// Both metrics carry a "pod" label set to os.Hostname() (which equals the pod
// name in K8s). Because multiple replicas push to the same Pushgateway job,
// the pod label is also used as a grouping key so each replica's series has its
// own slot and replicas do not overwrite each other.
type Pusher struct {
	BaseURL string // e.g. "http://pushgateway.observability:9091"
	Job     string // e.g. "crucible_provision_worker"
	Pod     string // pod label value; defaults to hostname in NewPusher
	HTTP    *http.Client
}

// Push serializes the current state of elec and sends it to Pushgateway.
// A no-op when BaseURL is empty. Safe to call concurrently.
func (p *Pusher) Push(ctx context.Context, elec *Elector) error {
	if p.BaseURL == "" {
		return nil
	}
	body := serializeLeaderMetrics(p.Pod, elec.IsLeader(), elec.Transitions())
	target := leaderPushURL(p.BaseURL, p.Job, p.Pod)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("leader pusher: build request: %w", err)
	}
	req.Header.Set("Content-Type", "text/plain; version=0.0.4")
	client := p.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("leader pusher: push: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("leader pusher: pushgateway %d: %s", resp.StatusCode, string(buf))
	}
	return nil
}

// RunPusher pushes leader metrics every interval until ctx is cancelled. Errors
// are logged via the elector's logger field — call site should pass the worker
// logger. Designed to be launched as:
//
//	go pusher.RunPusher(ctx, elec, 30*time.Second, logger)
func (p *Pusher) RunPusher(ctx context.Context, elec *Elector, interval time.Duration) {
	if p.BaseURL == "" {
		return
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := p.Push(ctx, elec); err != nil && ctx.Err() == nil {
				elec.logger.Warn("leader metrics push failed", "error", err)
			}
		}
	}
}

func serializeLeaderMetrics(pod string, isLeader bool, transitions int64) []byte {
	var b bytes.Buffer
	isLeaderVal := 0
	if isLeader {
		isLeaderVal = 1
	}
	b.WriteString("# HELP crucible_worker_is_leader 1 if this worker pod currently holds the leader advisory lock, 0 otherwise.\n")
	b.WriteString("# TYPE crucible_worker_is_leader gauge\n")
	fmt.Fprintf(&b, "crucible_worker_is_leader{pod=%q} %d\n", pod, isLeaderVal)

	b.WriteString("# HELP crucible_worker_leader_transitions_total Cumulative count of leadership state changes (acquire + release events) for this pod.\n")
	b.WriteString("# TYPE crucible_worker_leader_transitions_total gauge\n")
	fmt.Fprintf(&b, "crucible_worker_leader_transitions_total{pod=%q} %d\n", pod, transitions)

	return b.Bytes()
}

func leaderPushURL(base, job, pod string) string {
	grouping := map[string]string{
		"component": "leader",
		"pod":       pod,
	}
	var b strings.Builder
	b.WriteString(strings.TrimRight(base, "/"))
	b.WriteString("/metrics/job/")
	b.WriteString(url.PathEscape(job))
	keys := make([]string, 0, len(grouping))
	for k := range grouping {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString("/")
		b.WriteString(url.PathEscape(k))
		b.WriteString("/")
		b.WriteString(url.PathEscape(grouping[k]))
	}
	return b.String()
}
