package runner_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jmal1/selfservice-api/internal/runner"
)

// TestMillis_MarshalsAsMilliseconds is the guard for the unit bug that shipped:
// ActionOutput.Duration was a plain time.Duration tagged `json:"duration_ms"`,
// and encoding/json writes time.Duration as its raw int64 nanosecond count. A
// 30ms action serialised as 30000000 under a field named "_ms", which the UI's
// formatDuration() rendered as "30000.0s".
//
// The assertion is deliberately on the *exact integer*, not on "is a number" —
// the old behaviour also produced a valid number.
func TestMillis_MarshalsAsMilliseconds(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want string
	}{
		{"whole seconds", 30 * time.Second, "30000"},
		{"milliseconds", 30 * time.Millisecond, "30"},
		{"zero", 0, "0"},
		{"sub-millisecond truncates", 999 * time.Microsecond, "0"},
		{"minutes", 2 * time.Minute, "120000"},
		{"the value that shipped wrong", 1234 * time.Millisecond, "1234"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(runner.FromDuration(tc.in))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("Millis(%v) marshalled as %s, want %s\n"+
					"A value ~1e6 too large means the field reverted to a raw time.Duration.",
					tc.in, got, tc.want)
			}
		})
	}
}

// TestMillis_RoundTripsThroughJSON guards the marshal/unmarshal pair. The runner
// POSTs these payloads to the engine, which parses them back, so an asymmetric
// pair would rescale every recorded duration by a factor of a million on the way
// in — and would still look perfectly plausible in isolation.
func TestMillis_RoundTripsThroughJSON(t *testing.T) {
	for _, d := range []time.Duration{
		0, time.Millisecond, 250 * time.Millisecond, 30 * time.Second, 15 * time.Minute,
	} {
		raw, err := json.Marshal(runner.FromDuration(d))
		if err != nil {
			t.Fatalf("marshal %v: %v", d, err)
		}
		var back runner.Millis
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		if back.Duration() != d {
			t.Errorf("round trip of %v produced %v (wire: %s)", d, back.Duration(), raw)
		}
	}
}

// TestActionOutput_DurationFieldIsMilliseconds pins the unit at the struct that
// actually crosses the wire, so a future refactor that swaps the field type back
// to time.Duration fails here even if Millis itself keeps working.
func TestActionOutput_DurationFieldIsMilliseconds(t *testing.T) {
	raw, err := json.Marshal(runner.ActionOutput{
		Action:   "http_responds",
		Status:   "pass",
		Duration: runner.FromDuration(30 * time.Second),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"duration_ms":30000`) {
		t.Errorf("ActionOutput.Duration is not serialising as milliseconds.\ngot: %s", raw)
	}
	if strings.Contains(string(raw), `"duration_ms":30000000000`) {
		t.Errorf("ActionOutput.Duration reverted to raw nanoseconds.\ngot: %s", raw)
	}
}

// TestWorkflowRunResult_TotalDurationIsMilliseconds does the same for the
// workflow-level field, which the results page renders at the top of each
// workflow section.
func TestWorkflowRunResult_TotalDurationIsMilliseconds(t *testing.T) {
	raw, err := json.Marshal(runner.WorkflowRunResult{
		WorkflowSlug:  "checks/https",
		Status:        "pass",
		TotalDuration: runner.FromDuration(90 * time.Second),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"total_duration_ms":90000`) {
		t.Errorf("TotalDuration is not serialising as milliseconds.\ngot: %s", raw)
	}
}

// TestMillis_RejectsNonInteger keeps the failure loud. Silently defaulting a
// malformed duration to zero would make a broken runner look instantaneous
// rather than broken.
func TestMillis_RejectsNonInteger(t *testing.T) {
	var m runner.Millis
	if err := json.Unmarshal([]byte(`"30s"`), &m); err == nil {
		t.Error("expected an error for a non-integer duration, got nil")
	}
}
