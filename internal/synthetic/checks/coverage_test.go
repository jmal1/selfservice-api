package checks

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jmal1/selfservice-api/internal/synthetic"
)

func TestPodLifecycleEnabled_CoverageSignal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		check   synthetic.Check
		wantErr string
	}{
		{
			name:    "enabled",
			check:   PodLifecycleEnabled(CoverageConfig{Enabled: true}),
			wantErr: "",
		},
		{
			name:    "disabled",
			check:   PodLifecycleEnabled(CoverageConfig{Enabled: false}),
			wantErr: "SYNTHETIC_LIFECYCLE_ENABLED=false",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, err := tc.check.Run(context.Background(), synthetic.NewClient("http://x", ""))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if status != http.StatusOK {
					t.Fatalf("status=%d, want 200", status)
				}
				return
			}
			if err == nil {
				t.Fatal("expected failure")
			}
			if status != http.StatusServiceUnavailable {
				t.Fatalf("status=%d, want 503", status)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestRunnerSmokeEnabled_CoverageSignal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		check   synthetic.Check
		wantErr string
	}{
		{
			name:    "enabled",
			check:   RunnerSmokeEnabled(CoverageConfig{Enabled: true}),
			wantErr: "",
		},
		{
			name:    "disabled",
			check:   RunnerSmokeEnabled(CoverageConfig{Enabled: false}),
			wantErr: "SYNTHETIC_RUNNER_EXPECTED_ENABLED=false",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, err := tc.check.Run(context.Background(), synthetic.NewClient("http://x", ""))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if status != http.StatusOK {
					t.Fatalf("status=%d, want 200", status)
				}
				return
			}
			if err == nil {
				t.Fatal("expected failure")
			}
			if status != http.StatusServiceUnavailable {
				t.Fatalf("status=%d, want 503", status)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestCoverageChecks_Metadata(t *testing.T) {
	for _, c := range []synthetic.Check{
		PodLifecycleEnabled(CoverageConfig{Enabled: true}),
		RunnerSmokeEnabled(CoverageConfig{Enabled: true}),
	} {
		if strings.TrimSpace(c.Title()) == "" || strings.TrimSpace(c.Description()) == "" {
			t.Fatalf("%s missing dashboard metadata", c.Name())
		}
		if c.Severity() != synthetic.SeverityWarning {
			t.Fatalf("%s severity = %q, want warning", c.Name(), c.Severity())
		}
		if c.Runbook() == "" {
			t.Fatalf("%s missing runbook", c.Name())
		}
	}
}
