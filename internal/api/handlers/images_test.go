package handlers

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jmal1/selfservice-api/internal/models"
)

func TestImageKindFromFilename(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		want     string
		wantErr  bool
	}{
		{"iso lower", "kali-2024.4-installer-amd64.iso", models.ImageKindISO, false},
		{"iso upper", "WINDOWS.ISO", models.ImageKindISO, false},
		{"iso mixed", "Ubuntu-24.04.Iso", models.ImageKindISO, false},
		{"ova", "appliance.ova", models.ImageKindOVA, false},
		{"ova upper", "APPLIANCE.OVA", models.ImageKindOVA, false},
		{"leading space tolerated", "  thing.iso  ", models.ImageKindISO, false},
		// Bare .ovf is useless without its sibling VMDKs — reject early
		// rather than failing confusingly at import time.
		{"bare ovf rejected", "descriptor.ovf", "", true},
		{"exe rejected", "payload.exe", "", true},
		{"vmdk rejected", "disk.vmdk", "", true},
		{"no extension", "kali", "", true},
		{"empty", "", "", true},
		{"extension only", ".iso", models.ImageKindISO, false},
		// Double extension must resolve on the LAST one, so
		// "evil.iso.exe" is an exe and is rejected.
		{"double extension uses last", "evil.iso.exe", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ImageKindFromFilename(tc.filename)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got kind %q", tc.filename, got)
				}
				if !errors.Is(err, ErrUnsupportedImageKind) {
					t.Fatalf("expected ErrUnsupportedImageKind, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.filename, err)
			}
			if got != tc.want {
				t.Fatalf("kind = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSanitizeImageFilename(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"plain", "kali.iso", "kali.iso"},
		{"keeps dots dashes underscores", "kali-2024.4_amd64.iso", "kali-2024.4_amd64.iso"},
		{"spaces become dashes", "Windows Server 2025.iso", "Windows-Server-2025.iso"},
		{"unix traversal", "../../etc/passwd", "passwd"},
		{"windows traversal", `..\..\Windows\System32\cmd.exe`, "cmd.exe"},
		{"absolute unix path", "/var/lib/secret.iso", "secret.iso"},
		{"absolute windows path", `C:\images\thing.iso`, "thing.iso"},
		{"mixed separators", `foo/bar\baz.iso`, "baz.iso"},
		{"bare dotdot", "..", "image"},
		{"single dot", ".", "image"},
		{"empty", "", "image"},
		{"only bad chars", "!@#$%^&*()", "image"},
		{"strips quotes", `ka"li'.iso`, "kali.iso"},
		{"drops unicode", "kali-日本語.iso", "kali-.iso"},
		{"drops null byte", "kali\x00.iso", "kali.iso"},
		{"drops newline", "kali\n.iso", "kali.iso"},
		{"leading dots trimmed", "...hidden.iso", "hidden.iso"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeImageFilename(tc.input)
			if got != tc.want {
				t.Fatalf("SanitizeImageFilename(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// The whole point of sanitization: no input may produce a key that
// escapes the crucible/ prefix the MinIO service-account policy is
// scoped to.
func TestImageObjectKey_NeverEscapesPrefix(t *testing.T) {
	nasty := []string{
		"../../etc/passwd",
		`..\..\..\windows\system32\config\sam`,
		"/absolute/path.iso",
		"....//....//escape.iso",
		"a/../../b.iso",
		strings.Repeat("../", 50) + "deep.iso",
		"..",
		"",
		"normal.iso",
	}
	const id = "11111111-2222-3333-4444-555555555555"
	for _, in := range nasty {
		key := ImageObjectKey(id, in)

		if !strings.HasPrefix(key, "crucible/"+id+"/") {
			t.Errorf("key %q for input %q escaped the expected prefix", key, in)
		}
		if strings.Contains(key, "..") {
			t.Errorf("key %q for input %q contains a traversal sequence", key, in)
		}
		if strings.Count(key, "/") != 2 {
			t.Errorf("key %q for input %q has extra path separators", key, in)
		}
		if strings.Contains(key, `\`) {
			t.Errorf("key %q for input %q contains a backslash", key, in)
		}
	}
}

func TestSanitizeImageFilename_IsIdempotent(t *testing.T) {
	inputs := []string{
		"Windows Server 2025.iso", "../../etc/passwd", "kali-日本語.iso",
		"...hidden.iso", "", "!@#$", `C:\images\thing.iso`,
	}
	for _, in := range inputs {
		once := SanitizeImageFilename(in)
		twice := SanitizeImageFilename(once)
		if once != twice {
			t.Errorf("not idempotent for %q: %q -> %q", in, once, twice)
		}
	}
}

func TestSanitizeImageFilename_BoundsLength(t *testing.T) {
	got := SanitizeImageFilename(strings.Repeat("a", 500) + ".iso")
	if len(got) > 120 {
		t.Fatalf("length %d exceeds the 120-char bound", len(got))
	}
}

func TestPlanParts(t *testing.T) {
	const mib = 1 << 20
	const gib = 1 << 30
	tests := []struct {
		name string
		size int64
		part int64
		want int
	}{
		{"10 MiB is one part", 10 * mib, ImagePartSizeBytes, 1},
		{"exactly one part size", ImagePartSizeBytes, ImagePartSizeBytes, 1},
		{"one byte over rolls to two", ImagePartSizeBytes + 1, ImagePartSizeBytes, 2},
		{"12 GiB at 64 MiB parts", 12 * gib, ImagePartSizeBytes, 192},
		{"16 GiB cap at 64 MiB parts", 16 * gib, ImagePartSizeBytes, 256},
		{"zero size still gets a part", 0, ImagePartSizeBytes, 1},
		{"negative size still gets a part", -5, ImagePartSizeBytes, 1},
		{"zero part size falls back to default", 12 * gib, 0, 192},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PlanParts(tc.size, tc.part); got != tc.want {
				t.Fatalf("PlanParts(%d, %d) = %d, want %d", tc.size, tc.part, got, tc.want)
			}
		})
	}
}

// Every part must be at least 5 MiB or S3 rejects the multipart
// completion for all but the final chunk.
func TestImagePartSize_MeetsS3Minimum(t *testing.T) {
	const s3MinPartSize = 5 << 20
	if ImagePartSizeBytes < s3MinPartSize {
		t.Fatalf("part size %d is below the S3 minimum of %d", ImagePartSizeBytes, s3MinPartSize)
	}
}

func TestValidateImageUploadSize(t *testing.T) {
	tests := []struct {
		name       string
		size       int64
		wantStatus int
		wantErr    bool
	}{
		{"normal", 4 << 30, http.StatusOK, false},
		{"exactly at cap", MaxImageUploadBytes, http.StatusOK, false},
		{"one over cap", MaxImageUploadBytes + 1, http.StatusRequestEntityTooLarge, true},
		{"way over cap", 100 << 30, http.StatusRequestEntityTooLarge, true},
		{"zero", 0, http.StatusBadRequest, true},
		{"negative", -1, http.StatusBadRequest, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, err := ValidateImageUploadSize(tc.size)
			if tc.wantErr && err == nil {
				t.Fatalf("expected an error for size %d", tc.size)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for size %d: %v", tc.size, err)
			}
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d", status, tc.wantStatus)
			}
		})
	}
}

// Phase 0 finding P0-4: MinIO sits on stagingv01's root filesystem with
// ~85 GB free, shared with apt-cacher-ng. This guards the cap against
// being casually raised back to the originally-planned 32 GiB.
func TestMaxImageUploadBytes_RespectsStagingDiskCeiling(t *testing.T) {
	const sixteenGiB = 16 << 30
	if MaxImageUploadBytes != sixteenGiB {
		t.Fatalf("upload cap is %d; expected 16 GiB (%d). MinIO shares an ~85 GB root "+
			"filesystem with apt-cacher-ng on stagingv01 — raising this needs a dedicated volume first",
			MaxImageUploadBytes, sixteenGiB)
	}
}

func TestPresignTTL_IsShort(t *testing.T) {
	if PresignTTLSeconds > 15*60 {
		t.Fatalf("presign TTL is %ds; must be <= 900s (a leaked URL is a write primitive into the bucket)", PresignTTLSeconds)
	}
}
