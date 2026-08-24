package checks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jmal1/selfservice-api/internal/synthetic"
)

func TestTemplateReplicaBuildStatusRBAC(t *testing.T) {
	for _, tt := range []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "student rejected", status: http.StatusForbidden},
		{name: "route open", status: http.StatusNotFound, wantErr: true},
		{name: "student allowed", status: http.StatusOK, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer server.Close()

			status, err := TemplateReplicaBuildStatusRBAC.Run(
				context.Background(),
				synthetic.NewClient(server.URL, ""),
			)
			if status != tt.status || (err != nil) != tt.wantErr {
				t.Fatalf("status=%d error=%v, want status=%d error=%t", status, err, tt.status, tt.wantErr)
			}
		})
	}
}
