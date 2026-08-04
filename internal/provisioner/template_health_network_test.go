package provisioner

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/jmal1/selfservice-api/internal/models"
)

// testStagingNetwork mirrors the real default (migration 000021 sets
// templates.staging_network DEFAULT 'PG-VM-Lab', and every active template in
// production carries exactly that value).
const testStagingNetwork = "PG-VM-Lab"

// TestDeepCheckClonesOntoTemplateStagingNetwork is the regression test for the
// second production failure of the template-health deep check.
//
// After the retry fix landed, the deep check was observed on 2026-08-04 making
// three attempts against "Ubuntu 24.04 Server": attempts 1 and 2 both cloned
// cleanly in ~40s and then sat for the full 10-minute deadline without ever
// obtaining an IP. That template had provisioned 72 student VMs successfully.
//
// The cause: runDeepCheck passed cfg.Network, a global config field that
// cmd/provision-worker never sets, so it was always "". With no network
// override a clone inherits the template's own NIC backing, and template VMs
// do not sit on a DHCP-served port group -- so WaitForIP could never succeed
// and the deep check could never pass for ANY template.
//
// The template-verify path (internal/provisioner/template_jobs.go) has always
// used tmpl.StagingNetwork, which is why verify worked and the deep check did
// not. The deep check must use the same network.
//
// Without the fix this test fails with `network = ""`.
func TestDeepCheckClonesOntoTemplateStagingNetwork(t *testing.T) {
	tmpl := makeTemplate("11111111-1111-1111-1111-111111111111", "vm-101")
	db := newFakeHealthDB([]models.Template{tmpl})
	vc := newFakeHealthVC()

	cfg := defaultCfg()
	// Deliberately leave cfg.Network empty: that is exactly how the worker
	// runs in production, and it is what made the bug invisible.
	cfg.Network = ""

	counts, err := reconcileTemplateHealth(context.Background(), db, vc, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)), cfg)
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if !counts.DeepChecked {
		t.Fatal("expected a deep check to have run")
	}

	if got := vc.lastCloneParams.Network; got != testStagingNetwork {
		t.Errorf("deep-check clone must attach to the template's staging network %q, got %q -- "+
			"a clone with no network override keeps the template's NIC backing, which has no DHCP, "+
			"so WaitForIP can never succeed and the deep check fails by timeout on a healthy template",
			testStagingNetwork, got)
	}
}

// TestDeepCheckFallsBackToConfiguredNetwork covers the degenerate case: a
// template with no staging_network recorded. Rather than clone onto no network
// at all (guaranteed timeout), fall back to the reconciler's configured
// network so an operator retains an escape hatch.
func TestDeepCheckFallsBackToConfiguredNetwork(t *testing.T) {
	tmpl := makeTemplate("11111111-1111-1111-1111-111111111111", "vm-101")
	tmpl.StagingNetwork = ""
	db := newFakeHealthDB([]models.Template{tmpl})
	vc := newFakeHealthVC()

	cfg := defaultCfg()
	cfg.Network = "fallback-portgroup"

	if _, err := reconcileTemplateHealth(context.Background(), db, vc, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)), cfg); err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}

	if got := vc.lastCloneParams.Network; got != "fallback-portgroup" {
		t.Errorf("expected fallback to cfg.Network when the template has none, got %q", got)
	}
}
