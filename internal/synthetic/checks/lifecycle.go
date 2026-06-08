package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jmal1/selfservice-api/internal/synthetic"
)

// SyntheticPodNamePrefix prefixes every pod created by the lifecycle check so
// the pre-clean step can identify orphan pods without depending on labels or
// the (currently absent) "internal template" flag. Anything starting with
// this prefix and owned by the synthetic user is fair game to destroy.
const SyntheticPodNamePrefix = "synthetic-noop-"

// PodLifecycleConfig controls the lifecycle check's behavior. Defaults are
// chosen to fit comfortably inside a 10-minute CronJob cadence with the
// existing Ubuntu 24.04 clone path (~5-7 min cold).
type PodLifecycleConfig struct {
	// TemplateName MUST match a row in templates.name (NOT vcenter_template).
	// Empty disables the check entirely; the runner won't register it.
	TemplateName string

	// ReadyTimeout is the upper bound for waiting on PodStatusActive.
	// 8 minutes accommodates a cold vCenter clone + customize + NetBird
	// onboarding with a safety margin.
	ReadyTimeout time.Duration

	// DestroyTimeout is the upper bound for waiting on PodStatusDestroyed.
	DestroyTimeout time.Duration

	// PreCleanMaxAge controls which orphan synthetic pods are pre-destroyed.
	// 5 minutes is shorter than a normal lifecycle run so legitimate in-flight
	// runs aren't trampled when overlap happens.
	PreCleanMaxAge time.Duration
}

// DefaultPodLifecycleConfig returns the production-tuned defaults.
func DefaultPodLifecycleConfig(templateName string) PodLifecycleConfig {
	return PodLifecycleConfig{
		TemplateName:   templateName,
		ReadyTimeout:   8 * time.Minute,
		DestroyTimeout: 90 * time.Second,
		PreCleanMaxAge: 5 * time.Minute,
	}
}

// PodLifecycle returns a Check that exercises the full pod create+destroy
// path against the live API: list templates → pre-clean orphan synthetic pods
// → POST /api/v1/pods → poll for active → DELETE → poll for destroyed.
//
// This is the most expensive check (5-8 minutes per cycle) and the most
// holistic: it catches breakage in the worker queue, vCenter clone path,
// NetBird onboarding webhook, and destroy path that no narrower check covers.
//
// The check name is stable: `pod_lifecycle`. Alert rules and dashboards
// reference this name directly.
func PodLifecycle(cfg PodLifecycleConfig) synthetic.Check {
	return synthetic.CheckFunc{
		NameVal:        "pod_lifecycle",
		TitleVal:       "Pod create + destroy (full lifecycle)",
		DescriptionVal: "POSTs a synthetic-noop pod, polls for status=active, DELETEs, and verifies destroyed. Single best signal for end-to-end provisioning health.",
		SeverityVal:    synthetic.SeverityCritical,
		RunFn: func(ctx context.Context, c *synthetic.Client) (int, error) {
			return runPodLifecycle(ctx, c, cfg)
		},
	}
}

// runPodLifecycle is the actual implementation, factored out for testability.
// All HTTP status returns reflect the LAST status seen so failures surface
// the offending response code in the metric.
func runPodLifecycle(ctx context.Context, c *synthetic.Client, cfg PodLifecycleConfig) (int, error) {
	// 1. Resolve template name -> UUID via the live /templates endpoint.
	tmplID, status, err := resolveTemplateID(ctx, c, cfg.TemplateName)
	if err != nil {
		return status, fmt.Errorf("resolve template %q: %w", cfg.TemplateName, err)
	}

	// 2. Pre-clean: destroy orphan synthetic pods so they don't accumulate
	// when a previous run was killed mid-flight.
	if status, err := preCleanOrphans(ctx, c, cfg.PreCleanMaxAge); err != nil {
		// Pre-clean failure is logged via the returned error but does NOT
		// fail the check on its own; the create+destroy below is the
		// primary assertion. We do, however, propagate the status.
		return status, fmt.Errorf("pre-clean (continuing): %w", err)
	}

	// 3. Create the pod.
	podID, status, err := createSyntheticPod(ctx, c, tmplID)
	if err != nil {
		return status, fmt.Errorf("create pod: %w", err)
	}

	// 4. Always attempt to destroy the pod, even on later failures, to keep
	// the lab tidy. Deferred so a polling timeout still triggers cleanup.
	defer func() {
		// Use a fresh context bounded by the destroy timeout: the outer ctx
		// may already be at its deadline.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), cfg.DestroyTimeout)
		defer cancel()
		_, _ = destroyPod(cleanupCtx, c, podID)
	}()

	// 5. Poll for active.
	if status, err := waitForPodStatus(ctx, c, podID, []string{"active"}, cfg.ReadyTimeout, 15*time.Second); err != nil {
		return status, fmt.Errorf("wait for active: %w", err)
	}

	// 6. Delete.
	if status, err := destroyPod(ctx, c, podID); err != nil {
		return status, fmt.Errorf("delete pod: %w", err)
	}

	// 7. Poll for destroyed.
	if status, err := waitForPodStatus(ctx, c, podID, []string{"destroyed"}, cfg.DestroyTimeout, 5*time.Second); err != nil {
		return status, fmt.Errorf("wait for destroyed: %w", err)
	}

	return http.StatusOK, nil
}

// resolveTemplateID looks up a template by NAME in /api/v1/templates.
// Returns the matching UUID, the last HTTP status, and any error.
func resolveTemplateID(ctx context.Context, c *synthetic.Client, name string) (string, int, error) {
	if strings.TrimSpace(name) == "" {
		return "", 0, fmt.Errorf("template name is empty")
	}
	resp, err := c.Do(ctx, http.MethodGet, "/api/v1/templates", nil)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", resp.StatusCode, fmt.Errorf("/templates returned %d: %s", resp.StatusCode, snippet(body))
	}
	var tmpls []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &tmpls); err != nil {
		return "", resp.StatusCode, fmt.Errorf("templates body not a JSON array: %w", err)
	}
	for _, t := range tmpls {
		if t.Name == name {
			return t.ID, resp.StatusCode, nil
		}
	}
	return "", resp.StatusCode, fmt.Errorf("template %q not found among %d accessible templates", name, len(tmpls))
}

// listMyPods returns the caller's pods as decoded by the synthetic monitor.
// We only need a subset of fields, so we decode loosely.
type listPodEntry struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

func listMyPods(ctx context.Context, c *synthetic.Client) ([]listPodEntry, int, error) {
	resp, err := c.Do(ctx, http.MethodGet, "/api/v1/pods", nil)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, fmt.Errorf("/pods returned %d: %s", resp.StatusCode, snippet(body))
	}
	var pods []listPodEntry
	if err := json.Unmarshal(body, &pods); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("pods body not a JSON array: %w", err)
	}
	return pods, resp.StatusCode, nil
}

// preCleanOrphans destroys any pod owned by the synthetic user whose name
// begins with SyntheticPodNamePrefix and whose CreatedAt is older than
// maxAge. Errors during deletion are logged via the returned error but
// each pod is attempted independently.
func preCleanOrphans(ctx context.Context, c *synthetic.Client, maxAge time.Duration) (int, error) {
	pods, status, err := listMyPods(ctx, c)
	if err != nil {
		return status, err
	}
	cutoff := time.Now().Add(-maxAge)
	var firstErr error
	for _, p := range pods {
		if !strings.HasPrefix(p.Name, SyntheticPodNamePrefix) {
			continue
		}
		if p.Status == "destroying" || p.Status == "destroyed" {
			continue
		}
		if !p.CreatedAt.IsZero() && p.CreatedAt.After(cutoff) {
			// Pod is recent enough that another active run may own it.
			continue
		}
		if delStatus, err := destroyPod(ctx, c, p.ID); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("destroy orphan %s: %w", p.ID, err)
				status = delStatus
			}
		}
	}
	return status, firstErr
}

// createSyntheticPod POSTs a new pod with a single VM cloned from templateID.
// Pod name embeds a timestamp so concurrent runs (unlikely but possible)
// don't collide on a pod_name unique constraint, if any future migration adds
// one.
func createSyntheticPod(ctx context.Context, c *synthetic.Client, templateID string) (string, int, error) {
	name := SyntheticPodNamePrefix + time.Now().UTC().Format("20060102t150405")
	body, _ := json.Marshal(map[string]any{
		"name": name,
		"vms": []map[string]any{
			{
				"template_id":  templateID,
				"display_name": "noop",
			},
		},
	})
	resp, err := c.Do(ctx, http.MethodPost, "/api/v1/pods", strings.NewReader(string(body)))
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return "", resp.StatusCode, fmt.Errorf("POST /pods returned %d: %s", resp.StatusCode, snippet(respBody))
	}
	var parsed struct {
		PodID string `json:"pod_id"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", resp.StatusCode, fmt.Errorf("create response not JSON: %w", err)
	}
	if parsed.PodID == "" {
		return "", resp.StatusCode, fmt.Errorf("create response missing pod_id: %s", snippet(respBody))
	}
	return parsed.PodID, resp.StatusCode, nil
}

// destroyPod issues DELETE /api/v1/pods/{id}. Treats 404 as success because
// some defer paths may double-fire (e.g. when waitForPodStatus already saw
// a destroyed terminal state).
func destroyPod(ctx context.Context, c *synthetic.Client, podID string) (int, error) {
	resp, err := c.Do(ctx, http.MethodDelete, "/api/v1/pods/"+podID, nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	switch resp.StatusCode {
	case http.StatusOK, http.StatusAccepted, http.StatusNoContent, http.StatusNotFound, http.StatusConflict:
		// Conflict means the pod is already being destroyed, which is fine.
		return resp.StatusCode, nil
	default:
		return resp.StatusCode, fmt.Errorf("DELETE /pods/%s returned %d: %s", podID, resp.StatusCode, snippet(body))
	}
}

// waitForPodStatus polls GET /api/v1/pods/{id} until pod.Status matches one
// of wantStatuses, the overall timeout expires, or an unrecoverable error
// occurs. The Pod's "error" status terminates the wait early with a failure.
func waitForPodStatus(ctx context.Context, c *synthetic.Client, podID string, wantStatuses []string, timeout, interval time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	// Cap interval to a fraction of the timeout so short timeouts (tests,
	// fast-fail configs) still get multiple poll attempts.
	if interval > timeout/3 && timeout/3 > 0 {
		interval = timeout / 3
	}
	if interval < 50*time.Millisecond {
		interval = 50 * time.Millisecond
	}
	var lastStatus int
	var lastPodStatus string
	for {
		resp, err := c.Do(ctx, http.MethodGet, "/api/v1/pods/"+podID, nil)
		if err != nil {
			return lastStatus, err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		lastStatus = resp.StatusCode

		if resp.StatusCode == http.StatusOK {
			var pod struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(body, &pod); err != nil {
				return resp.StatusCode, fmt.Errorf("pod body not JSON: %w", err)
			}
			lastPodStatus = pod.Status
			for _, want := range wantStatuses {
				if pod.Status == want {
					return resp.StatusCode, nil
				}
			}
			if pod.Status == "error" || pod.Status == "destroy_failed" {
				return resp.StatusCode, fmt.Errorf("pod entered terminal failure status %q", pod.Status)
			}
		} else if resp.StatusCode == http.StatusNotFound {
			// 404 can legitimately occur after destroy if the row is GC'd,
			// but in our schema it isn't — treat as terminal mismatch.
			for _, want := range wantStatuses {
				if want == "destroyed" {
					return resp.StatusCode, nil
				}
			}
			return resp.StatusCode, fmt.Errorf("pod %s not found before reaching %v", podID, wantStatuses)
		} else {
			return resp.StatusCode, fmt.Errorf("GET /pods/%s returned %d: %s", podID, resp.StatusCode, snippet(body))
		}

		if time.Now().After(deadline) {
			return lastStatus, fmt.Errorf("timed out waiting for status %v (last seen %q) after %s", wantStatuses, lastPodStatus, timeout)
		}

		select {
		case <-ctx.Done():
			return lastStatus, ctx.Err()
		case <-time.After(interval):
		}
	}
}
