package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

type sourceReplicaVCenterStub struct {
	VCenterConsole
	identity *vcenter.TemplateSourceIdentity
	err      error
}

func (s sourceReplicaVCenterStub) ResolveTemplateSourceIdentity(
	context.Context,
	string,
) (*vcenter.TemplateSourceIdentity, error) {
	return s.identity, s.err
}

func TestAdminCreateTemplateSourceReplicaPersistsResolvedIdentityAndPolicy(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to an isolated PostgreSQL database to run handler persistence tests")
	}
	if err := database.RunMigrations(dsn); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	templateID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO templates (id, name, vcenter_template, os_type)
		VALUES ($1, $2, $3, 'linux')
	`, templateID, "handler-source-"+templateID.String(), "legacy-"+templateID.String()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM templates WHERE id = $1`, templateID)
	})

	identity := &vcenter.TemplateSourceIdentity{
		SourceVMMoref:        "vm-2401",
		HostMoref:            "host-3401",
		HostName:             "nuc3.lab.jmal.io",
		ComputeResourceType:  "ClusterComputeResource",
		ComputeResourceMoref: "domain-c401",
		ComputeResourcePath:  "/LAB/host/Intel-Cluster",
	}
	queries := database.NewQueries(pool)
	h := NewHandler(
		queries,
		nil,
		sourceReplicaVCenterStub{identity: identity},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		nil,
	)

	create := func() *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(
			http.MethodPost,
			"/api/v1/admin/templates/"+templateID.String()+"/source-replicas",
			bytes.NewBufferString(`{"source_ref":"inventory-name-is-not-persisted"}`),
		)
		req = withRouteParam(req, "templateID", templateID)
		rec := httptest.NewRecorder()
		h.AdminCreateTemplateSourceReplica(rec, req)
		return rec
	}

	first := create()
	if first.Code != http.StatusCreated {
		t.Fatalf("first create status = %d, want 201: %s", first.Code, first.Body.String())
	}
	var created models.TemplateSourceReplica
	if err := json.Unmarshal(first.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created source replica: %v", err)
	}
	if created.SourceVMMoref != identity.SourceVMMoref ||
		created.ComputeResourceType != identity.ComputeResourceType ||
		created.ComputeResourceMoref != identity.ComputeResourceMoref ||
		created.ComputeResourcePath != identity.ComputeResourcePath ||
		created.Status != models.TemplateSourceReplicaReady {
		t.Fatalf("persisted replica = %+v, want resolved immutable identity %+v", created, identity)
	}

	duplicate := create()
	if duplicate.Code != http.StatusConflict {
		t.Fatalf("duplicate create status = %d, want 409: %s", duplicate.Code, duplicate.Body.String())
	}
	replicas, err := queries.ListTemplateSourceReplicas(ctx, templateID)
	if err != nil {
		t.Fatal(err)
	}
	if len(replicas) != 1 || replicas[0].ID != created.ID {
		t.Fatalf("replicas after duplicate = %+v, want only original %s", replicas, created.ID)
	}

	deleted, err := queries.DeleteTemplateSourceReplica(ctx, templateID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Fatal("delete persisted source replica = false, want true")
	}
	enabled, err := queries.TemplateSourceReplicaModeEnabled(ctx, templateID)
	if err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Fatal("source-replica policy disabled after deleting the last replica")
	}
}
