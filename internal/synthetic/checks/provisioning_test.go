package checks

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/jmal1/selfservice-api/internal/provisioning"
	"github.com/jmal1/selfservice-api/internal/synthetic"
)

func TestProvisioningStatusMatchesExpectedState(t *testing.T) {
	for _, expected := range []bool{true, false} {
		t.Run(fmt.Sprintf("enabled=%t", expected), func(t *testing.T) {
			want := provisioning.StatusFor(expected)
			srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
				"/api/v1/provisioning/status": func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"enabled":%t,"message":%q}`, want.Enabled, want.Message)
				},
			})
			check := ProvisioningStatus(ProvisioningStatusConfig{ExpectedEnabled: expected})
			status, err := check.Run(context.Background(), synthetic.NewClient(srv.URL, ""))
			if err != nil || status != http.StatusOK {
				t.Fatalf("status=%d err=%v", status, err)
			}
		})
	}
}

func TestProvisioningStatusRejectsStateOrMessageDrift(t *testing.T) {
	tests := []string{
		`{"enabled":true,"message":"Provisioning is temporarily unavailable for maintenance."}`,
		`{"enabled":false,"message":"internal incident details"}`,
	}
	for _, body := range tests {
		srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
			"/api/v1/provisioning/status": func(w http.ResponseWriter, _ *http.Request) {
				w.Write([]byte(body))
			},
		})
		check := ProvisioningStatus(ProvisioningStatusConfig{ExpectedEnabled: false})
		if _, err := check.Run(context.Background(), synthetic.NewClient(srv.URL, "")); err == nil {
			t.Fatalf("body %s unexpectedly satisfied the maintenance contract", body)
		}
	}
}
