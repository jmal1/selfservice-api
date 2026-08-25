package provisioning

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestVerifyImageRevisionFollowsGHCRBlobRedirect is a regression test for a
// proven production-deploy blocker: a guarded deploy dry-run resolved a
// correctly proven image digest, but deploy.sh's verify_image_revision read
// an EMPTY org.opencontainers.image.revision label from the runtime config
// blob. GHCR serves blob bytes from a redirect (in production, to Azure Blob
// Storage), not from ghcr.io itself; the config blob curl call was missing
// `--location`, so it silently received a zero-byte redirect response body
// instead of the runtime config JSON.
//
// This test extracts the actual `image_repository`, `canonical_image_repository`,
// and `verify_image_revision` function bodies out of deploy.sh (byte for
// byte, not reimplemented) and runs them with the REAL system curl against a
// mock two-host registry: a "ghcr.io" stand-in that 307-redirects blob reads
// to a second local endpoint, mimicking the real GHCR -> blob-storage
// handoff. It fails if a future change drops the redirect-following flags,
// and it proves the revision comparison itself still rejects a wrong or
// missing revision even when the redirect is followed correctly.
func TestVerifyImageRevisionFollowsGHCRBlobRedirect(t *testing.T) {
	requirePOSIXShell(t)
	realCurl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl not available")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available")
	}

	functionsSource := extractVerifyImageRevisionFunctions(t)
	if !strings.Contains(functionsSource, "--location") {
		t.Fatal("extracted verify_image_revision no longer requests --location; the GHCR blob redirect fix was removed")
	}

	const image = "ghcr.io/jmal1/selfservice-api-gateway@sha256:" + testDigestA

	for _, test := range []struct {
		name         string
		blobRevision string // baked into the redirected blob's JSON label; "" omits the label entirely
		sourceSHA    string
		wantSuccess  bool
	}{
		{
			name:         "matching revision resolved through redirect",
			blobRevision: testSourceSHA,
			sourceSHA:    testSourceSHA,
			wantSuccess:  true,
		},
		{
			name:         "wrong revision still fails after following redirect",
			blobRevision: otherSourceSHA,
			sourceSHA:    testSourceSHA,
			wantSuccess:  false,
		},
		{
			name:         "missing revision label still fails after following redirect",
			blobRevision: "",
			sourceSHA:    testSourceSHA,
			wantSuccess:  false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, runErr := runVerifyImageRevisionAgainstRedirectingRegistry(
				t, realCurl, functionsSource, image, test.sourceSHA, test.blobRevision,
			)
			if test.wantSuccess {
				if runErr != nil || !strings.Contains(output, "VERIFY_OK") {
					t.Fatalf("expected verify_image_revision to follow the redirect and succeed: %v\n%s", runErr, output)
				}
			} else if runErr == nil {
				t.Fatalf("expected verify_image_revision to fail, but it succeeded:\n%s", output)
			}
		})
	}
}

// extractVerifyImageRevisionFunctions pulls the exact, unmodified
// image_repository / canonical_image_repository / is_digest_image /
// verify_image_revision function bodies out of deploy.sh, from the marker
// function through (but not including) the next function. This avoids
// re-implementing curl flags in the test, which would silently stop
// exercising deploy.sh if the real script changed.
func extractVerifyImageRevisionFunctions(t *testing.T) string {
	t.Helper()
	deployScript, err := os.ReadFile(filepath.Join("..", "..", "deploy", "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	const startMarker = "\nimage_repository() {\n"
	const endMarker = "\nrequire_clean_source_tree() {\n"
	start := strings.Index(string(deployScript), startMarker)
	end := strings.Index(string(deployScript), endMarker)
	if start < 0 || end < 0 || end <= start {
		t.Fatal("could not locate verify_image_revision between its markers in deploy.sh")
	}
	functionsSource := string(deployScript)[start+1 : end]
	if !strings.Contains(functionsSource, "verify_image_revision() (") {
		t.Fatal("extracted block does not contain verify_image_revision; markers may be stale")
	}
	return functionsSource
}

// runVerifyImageRevisionAgainstRedirectingRegistry starts two local HTTPS
// servers: one standing in for ghcr.io's blob endpoint (always 307-redirects
// to the second server, exactly like GHCR proxying to Azure Blob Storage),
// and one standing in for that redirect target, which serves the runtime
// config JSON. It then runs the extracted verify_image_revision against
// them, using a thin curl shim that fakes the token/manifest round trips
// (no interesting behavior to exercise there) but forwards the blob request
// to the REAL curl binary with the exact flags deploy.sh constructed, so the
// redirect-following behavior under test is real.
func runVerifyImageRevisionAgainstRedirectingRegistry(
	t *testing.T,
	realCurl string,
	functionsSource string,
	image string,
	sourceSHA string,
	blobRevision string,
) (string, error) {
	t.Helper()
	root := t.TempDir()

	blobServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if blobRevision == "" {
			w.Write([]byte(`{"config":{"Labels":{}}}`))
			return
		}
		w.Write([]byte(`{"config":{"Labels":{"org.opencontainers.image.revision":` + strconv.Quote(blobRevision) + `}}}`))
	}))
	defer blobServer.Close()

	redirectServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, blobServer.URL+"/actual-blob", http.StatusTemporaryRedirect)
	}))
	defer redirectServer.Close()

	caBundlePath := filepath.Join(root, "ca-bundle.pem")
	var caBundle []byte
	for _, srv := range []*httptest.Server{redirectServer, blobServer} {
		caBundle = append(caBundle, pem.EncodeToMemory(&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: srv.Certificate().Raw,
		})...)
	}
	writeFile(t, caBundlePath, string(caBundle))

	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(binDir, "curl"), `#!/bin/bash
set -euo pipefail
args=("$@")
last=$(( ${#args[@]} - 1 ))
url="${args[$last]}"
case "$url" in
  *"ghcr.io/token"*)
    printf '{"token":"registry-token"}\n'
    ;;
  *"/manifests/"*)
    jq -cn --arg digest "$FAKE_CONFIG_DIGEST" \
      '{mediaType:"application/vnd.oci.image.manifest.v1+json",config:{digest:$digest}}'
    ;;
  *"/blobs/"*)
    args[$last]="$FAKE_BLOB_URL"
    exec "$REAL_CURL" --cacert "$FAKE_CA_BUNDLE" "${args[@]}"
    ;;
  *)
    echo "unexpected curl invocation: $*" >&2
    exit 98
    ;;
esac
`)
	writeExecutable(t, filepath.Join(binDir, "gh"), `#!/bin/bash
set -euo pipefail
if [ "$*" = "auth token" ]; then
  printf 'test-gh-token\n'
else
  echo "unexpected gh invocation: $*" >&2
  exit 97
fi
`)

	functionsPath := filepath.Join(root, "functions.sh")
	writeFile(t, functionsPath, functionsSource)

	driverPath := filepath.Join(root, "driver.sh")
	writeFile(t, driverPath, `set -euo pipefail
source "$FUNCTIONS_FILE"
SOURCE_OWNER=jmal1
verify_image_revision "$1" "$2"
printf 'VERIFY_OK\n'
`)

	cmd := exec.Command("bash", driverPath, image, sourceSHA)
	cmd.Env = append(
		os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"REAL_CURL="+realCurl,
		"FAKE_CA_BUNDLE="+caBundlePath,
		"FAKE_CONFIG_DIGEST=sha256:"+testDigestB,
		"FAKE_BLOB_URL="+redirectServer.URL+"/v2/jmal1/selfservice-api-gateway/blobs/sha256:"+testDigestB,
		"FUNCTIONS_FILE="+functionsPath,
	)
	output, runErr := cmd.CombinedOutput()
	return string(output), runErr
}
