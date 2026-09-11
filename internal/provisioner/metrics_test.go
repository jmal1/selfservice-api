package provisioner

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDestroyFailedPusher_Push_NoOpWhenBaseURLEmpty(t *testing.T) {
	p := &DestroyFailedPusher{}
	// No server stood up — if Push tries to dial anything, this test fails.
	if err := p.Push(context.Background(), 3); err != nil {
		t.Fatalf("expected nil err when BaseURL empty, got %v", err)
	}
}

func TestDestroyFailedPusher_Push_SerializesMetricsAndUsesGroupedURL(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := &DestroyFailedPusher{
		BaseURL:        srv.URL,
		Job:            "crucible_provision_worker",
		GroupingLabels: map[string]string{"layer": "api"},
		HTTP:           srv.Client(),
	}
	if err := p.Push(context.Background(), 2); err != nil {
		t.Fatalf("Push: %v", err)
	}

	if want := "/metrics/job/crucible_provision_worker/layer/api"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}

	for _, want := range []string{
		"# TYPE crucible_pods_destroy_failed_count gauge",
		"crucible_pods_destroy_failed_count 2",
		"# TYPE crucible_pods_destroy_failed_run_timestamp_seconds gauge",
		"crucible_pods_destroy_failed_run_timestamp_seconds ",
	} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("body missing %q\n--- body ---\n%s", want, gotBody)
		}
	}
}

func TestDestroyFailedPusher_Push_PropagatesNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("nope"))
	}))
	defer srv.Close()

	p := &DestroyFailedPusher{
		BaseURL: srv.URL,
		Job:     "crucible_provision_worker",
		HTTP:    srv.Client(),
	}
	err := p.Push(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("expected error containing 502, got %v", err)
	}
}

func TestDestroyFailedPushURL_DeterministicGroupingOrder(t *testing.T) {
	got := destroyFailedPushURL("http://pg:9091", "j", map[string]string{
		"layer": "api", "env": "prod",
	})
	// keys sorted alphabetically: env then layer
	want := "http://pg:9091/metrics/job/j/env/prod/layer/api"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
