package provisioner

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/database"
)

type capturedPush struct {
	method string
	body   string
}

func TestTemplateHealthSnapshotReplacementDropsDeletedAndStaleSeries(t *testing.T) {
	var mu sync.Mutex
	var pushes []capturedPush
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}

		mu.Lock()
		pushes = append(pushes, capturedPush{method: r.Method, body: string(body)})
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	now := time.Unix(1_700_000_000, 0)
	passed, failed := true, false
	duration := 12.5
	fault := "vsphere_virtual_disk"
	first := NewTemplateHealthPusher(server.URL, "worker", map[string]string{"layer": "api"})
	if err := first.ReplaceSnapshot(context.Background(), TemplateHealthSnapshot{
		CheckerUp: boolPtr(true),
		CreatedAt: now,
		States: []database.TemplateHealthState{
			{
				TemplateID:              uuid.New(),
				TemplateName:            "deleted-template",
				HealthStatus:            "unhealthy",
				LastDeepCheckAt:         &now,
				LastDeepPassed:          &failed,
				LastDeepDurationSeconds: &duration,
				LastDeepFaultClass:      &fault,
			},
			{
				TemplateID:            uuid.New(),
				TemplateName:          "kept-template",
				HealthStatus:          "healthy",
				LastStructuralCheckAt: &now,
				LastStructuralPassed:  &passed,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// A brand-new pusher simulates a worker restart. Its complete replacement
	// snapshot intentionally has neither the deleted template nor a deep attempt.
	restarted := NewTemplateHealthPusher(server.URL, "worker", map[string]string{"layer": "api"})
	if err := restarted.ReplaceSnapshot(context.Background(), TemplateHealthSnapshot{
		CreatedAt: now.Add(time.Minute),
		States: []database.TemplateHealthState{
			{
				TemplateID:            uuid.New(),
				TemplateName:          "kept-template",
				HealthStatus:          "healthy",
				LastStructuralCheckAt: &now,
				LastStructuralPassed:  &passed,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(pushes) != 2 {
		t.Fatalf("push count = %d, want 2", len(pushes))
	}
	for i, push := range pushes {
		if push.method != http.MethodPut {
			t.Fatalf("push %d method = %s, want PUT replacement semantics", i, push.method)
		}
	}
	if strings.Contains(pushes[1].body, "deleted-template") {
		t.Fatal("replacement snapshot retained a deleted template series")
	}
	if strings.Contains(pushes[1].body, `check_type="deep"`) ||
		strings.Contains(pushes[1].body, "vsphere_virtual_disk") {
		t.Fatal("restart replacement retained stale raw deep-attempt diagnostics")
	}
	if !strings.Contains(pushes[1].body, `crucible_template_health_status{template="kept-template"} 1`) {
		t.Fatal("replacement snapshot omitted current persisted confirmed state")
	}
}

func TestTemplateHealthCheckerFreshnessUsesOldestVisibleTemplate(t *testing.T) {
	oldest := time.Unix(1_700_000_000, 0)
	newest := oldest.Add(2 * time.Hour)
	passed := true
	body := string(serializeTemplateHealthSnapshot(TemplateHealthSnapshot{
		CreatedAt: newest,
		States: []database.TemplateHealthState{
			{TemplateName: "older", HealthStatus: "healthy", LastStructuralCheckAt: &oldest, LastStructuralPassed: &passed},
			{TemplateName: "newer", HealthStatus: "healthy", LastStructuralCheckAt: &newest, LastStructuralPassed: &passed},
		},
	}))
	want := "crucible_template_health_checker_last_success_timestamp_seconds 1700000000"
	if !strings.Contains(body, want) {
		t.Fatalf("freshness did not use the oldest all-template structural result; want %q in:\n%s", want, body)
	}

	body = string(serializeTemplateHealthSnapshot(TemplateHealthSnapshot{
		CreatedAt: newest,
		States: []database.TemplateHealthState{
			{TemplateName: "checked", HealthStatus: "healthy", LastStructuralCheckAt: &newest, LastStructuralPassed: &passed},
			{TemplateName: "new", HealthStatus: "unknown"},
		},
	}))
	if strings.Contains(body, "crucible_template_health_checker_last_success_timestamp_seconds ") {
		t.Fatal("freshness proof was emitted before every visible template had a structural result")
	}
}
