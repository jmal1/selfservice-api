package provisioner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPipelineMetrics_SerializeShape(t *testing.T) {
	m := NewPipelineMetrics("", "", nil)
	m.RecordImageUpload("iso", "created")
	m.RecordImageUpload("iso", "completed")
	m.RecordImageUpload("ova", "failed")
	m.RecordImageImport("iso", MetricResultSuccess, 90*time.Second, 4096)
	m.SetImageUploadsStuck(2)
	m.RecordTemplateTransition("ready", "verifying")
	m.RecordTemplateVerify("pass")
	m.RecordTemplateJob("template_provision", 30*time.Second)
	m.SetTemplateStates(map[string]int{"active": 5, "error": 1})
	m.SetTemplatesStuck(1)

	out := string(m.serialize())

	want := []string{
		`crucible_image_upload_total{kind="iso",result="created"} 1`,
		`crucible_image_upload_total{kind="iso",result="completed"} 1`,
		`crucible_image_upload_total{kind="ova",result="failed"} 1`,
		`crucible_image_import_total{kind="iso",result="success"} 1`,
		`crucible_image_import_duration_seconds_sum{kind="iso"} 90`,
		`crucible_image_import_duration_seconds_count{kind="iso"} 1`,
		`crucible_image_import_bytes_total{kind="iso"} 4096`,
		`crucible_image_uploads_stuck 2`,
		`crucible_template_transition_total{from="ready",to="verifying"} 1`,
		`crucible_template_verify_result_total{result="pass"} 1`,
		`crucible_template_job_duration_seconds_sum{job_type="template_provision"} 30`,
		`crucible_template_state{state="active"} 5`,
		`crucible_template_state{state="error"} 1`,
		`crucible_template_stuck 1`,
		"# TYPE crucible_image_upload_total counter",
		"# TYPE crucible_template_state gauge",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("serialized output missing %q\n--- got ---\n%s", w, out)
		}
	}
}

func TestPipelineMetrics_OmitsUncollectedStuckGauges(t *testing.T) {
	m := NewPipelineMetrics("", "", nil)
	out := string(m.serialize())

	for _, name := range []string{"crucible_image_uploads_stuck", "crucible_template_stuck"} {
		if strings.Contains(out, name) {
			t.Fatalf("expected %s to be omitted before collection:\n%s", name, out)
		}
	}
}

func TestPipelineMetrics_EmitsZeroStuckGaugesAfterCollection(t *testing.T) {
	m := NewPipelineMetrics("", "", nil)
	m.SetImageUploadsStuck(0)
	m.SetTemplatesStuck(0)
	out := string(m.serialize())

	for _, want := range []string{"crucible_image_uploads_stuck 0", "crucible_template_stuck 0"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q after collection:\n%s", want, out)
		}
	}
}

// Guards the label-escaping helper against the double-escaping bug that
// %q would introduce (it would emit \"iso\" instead of "iso").
func TestPipelineMetrics_LabelValuesNotDoubleEscaped(t *testing.T) {
	m := NewPipelineMetrics("", "", nil)
	m.RecordImageUpload(`we"ird`, "created")
	out := string(m.serialize())

	if strings.Contains(out, `\\"`) {
		t.Fatalf("label value was double-escaped:\n%s", out)
	}
	if !strings.Contains(out, `kind="we\"ird"`) {
		t.Fatalf("expected single-escaped quote in label value:\n%s", out)
	}
}

// An empty family must still emit HELP/TYPE but no samples, so Prometheus
// doesn't see a malformed exposition.
func TestPipelineMetrics_EmptyFamilyEmitsNoSamples(t *testing.T) {
	m := NewPipelineMetrics("", "", nil)
	out := string(m.serialize())

	if !strings.Contains(out, "# TYPE crucible_image_import_total counter") {
		t.Fatal("expected TYPE line for empty family")
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "crucible_image_import_total{") {
			t.Fatalf("empty family emitted a sample: %q", line)
		}
	}
}

func TestPipelineMetrics_PushNoopWithoutBaseURL(t *testing.T) {
	m := NewPipelineMetrics("", "", nil)
	if err := m.Push(context.Background()); err != nil {
		t.Fatalf("expected no-op push to succeed, got %v", err)
	}
}

func TestPipelineMetrics_PushPostsBody(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		buf := make([]byte, 1<<16)
		n, _ := r.Body.Read(buf)
		got = string(buf[:n])
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := NewPipelineMetrics(srv.URL, "crucible_pipeline", nil)
	m.RecordTemplateVerify("fail")
	if err := m.Push(context.Background()); err != nil {
		t.Fatalf("push: %v", err)
	}
	if !strings.Contains(got, `crucible_template_verify_result_total{result="fail"} 1`) {
		t.Fatalf("pushed body missing sample:\n%s", got)
	}
}

func TestPipelineMetrics_PushSurfacesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("text format parsing error"))
	}))
	defer srv.Close()

	m := NewPipelineMetrics(srv.URL, "crucible_pipeline", nil)
	if err := m.Push(context.Background()); err == nil {
		t.Fatal("expected error on non-2xx response")
	}
}
