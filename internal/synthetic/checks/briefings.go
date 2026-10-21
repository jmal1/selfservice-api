package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/jmal1/selfservice-api/internal/synthetic"
)

// BriefingAdminRBAC asserts students cannot read briefing drafts or history.
// The fixed nonexistent template UUID means an open route answers 404, which
// fails: authorization must reject the student before the owner lookup.
var BriefingAdminRBAC = synthetic.CheckFunc{
	NameVal:        "briefing_admin_rbac",
	TitleVal:       "RBAC: Student Cannot Read Briefing Drafts",
	DescriptionVal: "Calls the instructor briefing endpoint under /api/v1/admin/templates as a student-role user and requires 403. Unpublished briefing drafts and history must never reach students.",
	SeverityVal:    synthetic.SeverityWarning,
	RunFn: func(ctx context.Context, c *synthetic.Client) (int, error) {
		const path = "/api/v1/admin/templates/00000000-0000-0000-0000-000000000001/briefing/revisions"
		resp, err := c.Do(ctx, http.MethodGet, path, nil)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode == http.StatusForbidden {
			return resp.StatusCode, nil
		}
		return resp.StatusCode, fmt.Errorf(
			"briefing revisions returned %d to a student, want 403; the instructor RBAC gate is open or the route moved",
			resp.StatusCode)
	},
}

// BriefingPodContract proves the student briefing route is registered and
// answers its contract. An unknown pod must be 404 (not 200/405), and the
// student's own pod, when it has one, must be 200 with a sections array.
// The non-owner 403 cannot be probed here (the synthetic only knows its own
// pods); TestBriefingHandlersPostgres proves it.
var BriefingPodContract = synthetic.CheckFunc{
	NameVal:        "briefing_pod_contract",
	TitleVal:       "Pod Briefing Endpoint",
	DescriptionVal: "Calls /api/v1/pods/{id}/briefing for an unknown pod (requires 404) and for the synthetic user's own pod when one exists (requires 200 with a sections array).",
	SeverityVal:    synthetic.SeverityWarning,
	RunFn: func(ctx context.Context, c *synthetic.Client) (int, error) {
		status, body, err := getBody(ctx, c, "/api/v1/pods/00000000-0000-0000-0000-000000000001/briefing")
		if err != nil {
			return 0, err
		}
		if status != http.StatusNotFound {
			return status, fmt.Errorf("briefing for an unknown pod returned %d, want 404: %s", status, snippet(body))
		}

		status, body, err = getBody(ctx, c, "/api/v1/pods")
		if err != nil {
			return 0, err
		}
		var pods []struct {
			ID string `json:"id"`
		}
		if status != http.StatusOK || json.Unmarshal(body, &pods) != nil {
			return status, fmt.Errorf("could not list own pods (%d): %s", status, snippet(body))
		}
		if len(pods) == 0 {
			return status, nil
		}
		status, body, err = getBody(ctx, c, "/api/v1/pods/"+pods[0].ID+"/briefing")
		if err != nil {
			return 0, err
		}
		var out struct {
			Sections []json.RawMessage `json:"sections"`
		}
		if status != http.StatusOK || json.Unmarshal(body, &out) != nil || out.Sections == nil {
			return status, fmt.Errorf("briefing for own pod returned %d, want 200 with a sections array: %s", status, snippet(body))
		}
		return status, nil
	},
}

func getBody(ctx context.Context, c *synthetic.Client, path string) (int, []byte, error) {
	resp, err := c.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, nil
}

// BriefingAssetRBAC asserts students cannot upload briefing images and that
// the image read route exists for them. The upload probe sends an empty body,
// so even an open gate stores nothing (it would answer 400). It is a POST, so
// it stays out of ReadOnly() like image_upload_rbac.
var BriefingAssetRBAC = synthetic.CheckFunc{
	NameVal:        "briefing_asset_rbac",
	TitleVal:       "RBAC: Student Cannot Upload Briefing Images",
	DescriptionVal: "POSTs an empty body to /api/v1/admin/briefing-assets as a student-role user and requires 403, then requires 404 for an unknown /api/v1/briefing-assets/{sha256} so the student read route is proven to exist.",
	SeverityVal:    synthetic.SeverityWarning,
	RunFn: func(ctx context.Context, c *synthetic.Client) (int, error) {
		resp, err := c.Do(ctx, http.MethodPost, "/api/v1/admin/briefing-assets", nil)
		if err != nil {
			return 0, err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			return resp.StatusCode, fmt.Errorf("briefing image upload returned %d to a student, want 403", resp.StatusCode)
		}
		status, body, err := getBody(ctx, c, "/api/v1/briefing-assets/"+strings.Repeat("0", 64))
		if err != nil {
			return 0, err
		}
		if status != http.StatusNotFound {
			return status, fmt.Errorf("unknown briefing image returned %d to a student, want 404: %s", status, snippet(body))
		}
		return http.StatusForbidden, nil
	},
}
