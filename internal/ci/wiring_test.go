package ci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This guard exists because the same defect was shipped independently five
// times during the image-upload/Kali-runner work: a component was built and
// unit-tested in isolation, but never actually ATTACHED in the cmd/ main that
// runs in production.
//
// It is invisible to ordinary tests by construction. Unit tests inject the
// dependency directly (a fake store, a fake metrics recorder), so they pass
// whether or not main() wires the real one. The observable symptoms only show
// up in prod, and they are all quiet ones:
//
//   - Metrics recorders that are never attached leave every Record*/Set* call
//     a nil-receiver no-op, and a recorder that is attached but never flushed
//     never reaches Pushgateway. On a dashboard an ABSENT series and a ZERO
//     series look identical, so "no cleanup failures" is indistinguishable
//     from "the engine never ran".
//   - A store that is never attached makes its whole feature answer 503
//     forever, while every test of that feature stays green.
//
// Matching on source text is crude, but it is the cheapest thing that fails
// loudly when the wiring is dropped, and the alternative (booting a full
// engine/gateway in a test) needs Postgres, NATS and vCenter.
//
// If you intentionally remove one of these, delete its row AND say why.
var requiredWiring = map[string][]struct {
	symbol string
	why    string
}{
	"cmd/api-gateway/main.go": {
		{"WithImageStore", "without it every /admin/images route answers 503 and image upload is dead in prod"},
		{"WithVCenterISOs", "without it the template wizard's ISO picker is permanently empty"},
	},
	"cmd/crucible-engine/main.go": {
		{"NewRunnerMetrics", "without it e.metrics stays nil and every runner metric call is a silent no-op"},
		{"WithRunnerMetrics", "the recorder must be attached to BOTH the engine and the K8s client"},
		{"RunPusher", "recorded counters never reach Pushgateway without the flush loop"},
	},
	"cmd/provision-worker/main.go": {
		{"RunPusher", "image-import metrics never reach Pushgateway without the flush loop"},
		{"RunStuckUploadReconciler", "without it crucible_image_uploads_stuck is never refreshed, so leaked uploads are never detected"},
	},
	"cmd/crucible-runner/main.go": {
		{"MaterializeActionLibrary", "without it the engine-generated action library is never written to disk, so every library action (http_get, port_open, ssh_exec, …) fails with exit 127 — the original defect, in which workflows appeared to run, the Job exited 0, and no action could possibly pass"},
	},
	"cmd/synthetic-api-monitor/main.go": {
		{"checks.Elevated", "without it the instructor-role checks are never registered, so the authenticated admin surface (/admin/images, /admin/vcenter/isos, wizard-state) is unmonitored — and a 503 from an unwired dependency looks identical to a healthy deploy, because every other admin check only asserts a student is refused"},
		{"SYNTHETIC_INSTRUCTOR_USER_ID", "the elevated client must be minted from a dedicated instructor row; elevating the primary synthetic user instead would turn five RBAC checks into tautologies that pass while proving nothing"},
	},
	// Not a cmd/ main, but the same failure class: buildActionLibrary is a
	// package-level func, so deleting its only call site still compiles and
	// still passes every actionlibrary_test.go case (they call it directly).
	// The feature would just silently stop shipping.
	"internal/engine/engine.go": {
		{"ListRunnerLibraryActions", "without it no library bodies are fetched and the runner is provisioned with an empty library"},
		{"buildActionLibrary", "without it ActionLibrary is never populated on RunnerSpec and library actions revert to exit 127"},
	},
}

func TestCmdMains_WireOptionalDependencies(t *testing.T) {
	root := findRepoRoot(t)

	for relPath, required := range requiredWiring {
		t.Run(relPath, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relPath)))
			if err != nil {
				t.Fatalf("read %s: %v", relPath, err)
			}
			src := string(data)

			for _, req := range required {
				if !strings.Contains(src, req.symbol) {
					t.Errorf(
						"%s does not reference %q.\n"+
							"Why this matters: %s.\n"+
							"The feature's own unit tests inject dependencies directly, so they "+
							"will stay green while production silently does nothing.",
						relPath, req.symbol, req.why,
					)
				}
			}
		})
	}
}
