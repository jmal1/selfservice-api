package checks

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/jmal1/selfservice-api/internal/synthetic"
)

// ImageUploadRBAC asserts that the student-role synthetic user is refused by
// the image-upload surface.
//
// This is the highest-value check on the /admin/images block. The block was
// added late (migration 000023) and hangs off the shared /admin router; the
// specific way it could break is a chi Mount-ordering change that lands the
// route outside the RequireRole(instructor) guard, which is exactly the trap
// documented at routes.go:151-156. A student who can POST here can stage
// arbitrary multi-GB objects into MinIO on stagingv01, whose root filesystem
// is shared with apt-cacher-ng — so the blast radius of a permission
// regression is "students can fill the disk that every Linux template build
// depends on", not merely "students see an admin page".
//
// The request body is deliberately well-formed. A malformed body would let a
// 400 from the handler masquerade as a pass if the middleware ever stopped
// running, and this check exists precisely to detect that the middleware
// stopped running.
var ImageUploadRBAC = synthetic.CheckFunc{
	NameVal:        "image_upload_rbac",
	TitleVal:       "RBAC: Student Cannot Upload Images",
	DescriptionVal: "POSTs a well-formed image-upload request as the student-role synthetic user and requires 403. Guards the /admin/images block against falling outside the instructor role gate.",
	SeverityVal:    synthetic.SeverityCritical,
	RunFn: func(ctx context.Context, c *synthetic.Client) (int, error) {
		body := strings.NewReader(`{"filename":"synthetic-rbac-probe.iso","kind":"iso","size_bytes":1024}`)
		resp, err := c.Do(ctx, http.MethodPost, "/api/v1/admin/images", body)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)

		switch resp.StatusCode {
		case http.StatusForbidden:
			return resp.StatusCode, nil
		case http.StatusOK, http.StatusCreated, http.StatusAccepted:
			return resp.StatusCode, fmt.Errorf(
				"student-role user was ALLOWED to create an image upload (status %d) — the /admin/images RBAC gate is open",
				resp.StatusCode)
		case http.StatusNotFound:
			return resp.StatusCode, fmt.Errorf(
				"POST /api/v1/admin/images returned 404 — the route is missing, so this check is no longer proving anything about RBAC")
		default:
			return resp.StatusCode, fmt.Errorf(
				"POST /api/v1/admin/images returned %d as a student, want 403", resp.StatusCode)
		}
	},
}
