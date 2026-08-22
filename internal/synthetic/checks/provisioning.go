package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/jmal1/selfservice-api/internal/provisioning"
	"github.com/jmal1/selfservice-api/internal/synthetic"
)

// ProvisioningStatusConfig makes the expected maintenance state explicit.
type ProvisioningStatusConfig struct {
	ExpectedEnabled bool
}

// ProvisioningStatus verifies the authenticated, read-only admission contract.
func ProvisioningStatus(cfg ProvisioningStatusConfig) synthetic.Check {
	return synthetic.CheckFunc{
		NameVal:        "provisioning_status",
		TitleVal:       "Provisioning Maintenance State",
		DescriptionVal: "Calls the authenticated provisioning status endpoint and verifies its enabled flag and public message match the configured deployment expectation.",
		SeverityVal:    synthetic.SeverityCritical,
		RunFn: func(ctx context.Context, c *synthetic.Client) (int, error) {
			resp, err := c.Do(ctx, http.MethodGet, "/api/v1/provisioning/status", nil)
			if err != nil {
				return 0, err
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			if resp.StatusCode != http.StatusOK {
				return resp.StatusCode, fmt.Errorf("provisioning_status returned %d: %s", resp.StatusCode, snippet(body))
			}
			var got provisioning.Status
			if err := json.Unmarshal(body, &got); err != nil {
				return resp.StatusCode, fmt.Errorf("provisioning_status body is not JSON: %w", err)
			}
			want := provisioning.StatusFor(cfg.ExpectedEnabled)
			if got != want {
				return resp.StatusCode, fmt.Errorf("provisioning_status = %+v, want %+v", got, want)
			}
			return resp.StatusCode, nil
		},
	}
}
