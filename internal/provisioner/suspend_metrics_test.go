package provisioner

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestSuspendMetrics_LastRunSeededAtConstruction is the regression test for a
// false-positive alert: crucible_idle_evaluator_last_run_timestamp was pushed
// as 0 before the evaluator's first tick, so
// `time() - gauge > 1h` fired on EVERY worker restart and stayed lit until the
// first pass -- up to a full 15m interval after each deploy.
//
// The gauge must be present (an absent series can never satisfy a "too old"
// alert, so dropping it would be worse) AND recent at construction.
func TestSuspendMetrics_LastRunSeededAtConstruction(t *testing.T) {
	before := time.Now().Add(-2 * time.Second)
	m := NewSuspendMetrics("", "crucible_test", nil)
	after := time.Now().Add(2 * time.Second)

	body := string(m.serialize())

	const name = "crucible_idle_evaluator_last_run_timestamp"
	var line string
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, name+" ") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatalf("%s is absent from the push body; a \"value too old\" alert cannot "+
			"match an absent series, so it must always be emitted.\nbody:\n%s", name, body)
	}

	var got float64
	if _, err := fmt.Sscanf(line, name+" %g", &got); err != nil {
		t.Fatalf("could not parse %q: %v", line, err)
	}
	if got < float64(before.Unix()) || got > float64(after.Unix()) {
		t.Errorf("%s = %v, want a fresh timestamp in [%d, %d]. A zero/stale value makes "+
			"CrucibleIdleEvaluatorStale fire on every worker restart.",
			name, got, before.Unix(), after.Unix())
	}
}
