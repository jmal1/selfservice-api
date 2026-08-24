package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

type sourceReplicaVCenterStub struct {
	VCenterConsole
	identity *vcenter.TemplateSourceIdentity
	target   *vcenter.ReplicaBuildTarget
	err      error
}

func (s sourceReplicaVCenterStub) ResolveTemplateSourceIdentity(
	context.Context,
	string,
) (*vcenter.TemplateSourceIdentity, error) {
	return s.identity, s.err
}

func (s sourceReplicaVCenterStub) ResolveReplicaBuildTarget(
	context.Context,
	vcenter.ReplicaBuildTarget,
) (*vcenter.ReplicaBuildTarget, error) {
	return s.target, s.err
}

func (s sourceReplicaVCenterStub) ValidateReplicaBuildPrivileges(
	context.Context,
	string,
	vcenter.ReplicaBuildTarget,
) error {
	return s.err
}

func TestAdminCreateTemplateReplicaBuildIsIdempotentAndPersistsExactTarget(t *testing.T) {
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
	userID := uuid.New()
	if _, err := pool.Exec(ctx, `
			INSERT INTO users (id, oidc_sub, username, email)
			VALUES ($1, $2, $2, $3)
		`, userID, userID.String(), userID.String()+"@example.invalid"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
			INSERT INTO templates (id, name, vcenter_template, os_type)
			VALUES ($1, $2, $3, 'linux')
		`, templateID, "handler-build-"+templateID.String(), "legacy-"+templateID.String()); err != nil {
		t.Fatal(err)
	}
	queries := database.NewQueries(pool)
	now := time.Now().UTC()
	anchor := &models.TemplateSourceReplica{
		TemplateID:           templateID,
		SourceVMMoref:        "vm-4401",
		ComputeResourceType:  "ClusterComputeResource",
		ComputeResourceMoref: "domain-c4401",
		ComputeResourcePath:  "/DC/host/Source",
		Status:               models.TemplateSourceReplicaReady,
		LastValidatedAt:      &now,
	}
	if err := queries.CreateTemplateSourceReplica(ctx, anchor); err != nil {
		t.Fatal(err)
	}
	target := &vcenter.ReplicaBuildTarget{
		ComputeResourceType:  "ClusterComputeResource",
		ComputeResourceMoref: "domain-c4402",
		ComputeResourcePath:  "/DC/host/Intel",
		HostMoref:            "host-4402",
		HostName:             "outside-placement-allowlist.example.invalid",
		ResourcePoolMoref:    "resgroup-4402",
		ResourcePoolPath:     "/DC/host/Intel/Resources/Students",
		DatastoreMoref:       "datastore-4402",
		DatastoreName:        "replica-ds",
		FolderMoref:          "group-v4402",
		FolderPath:           "/DC/vm/Templates",
		ProvisionDatastore:   "student-ds",
	}
	h := NewHandler(
		queries,
		nil,
		sourceReplicaVCenterStub{target: target},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		nil,
	)
	body, err := json.Marshal(createTemplateReplicaBuildRequest{
		SourceReplicaID: anchor.ID,
		IdempotencyKey:  "intel-rollout-retained-v1",
		DestinationName: "template-intel-retained",
		Target:          *target,
	})
	if err != nil {
		t.Fatal(err)
	}
	create := func() *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(
			http.MethodPost,
			"/api/v1/admin/templates/"+templateID.String()+"/source-replica-builds",
			bytes.NewReader(body),
		)
		req = req.WithContext(middleware.WithUserID(req.Context(), userID))
		req = withRouteParam(req, "templateID", templateID)
		rec := httptest.NewRecorder()
		h.AdminCreateTemplateReplicaBuild(rec, req)
		return rec
	}
	first := create()
	if first.Code != http.StatusAccepted {
		t.Fatalf("first create status=%d body=%s", first.Code, first.Body.String())
	}
	h.vc = sourceReplicaVCenterStub{err: errors.New("vCenter unavailable after accepted response")}
	second := create()
	if second.Code != http.StatusOK {
		t.Fatalf("idempotent replay status=%d body=%s", second.Code, second.Body.String())
	}
	var firstBuild, secondBuild models.TemplateReplicaBuild
	if err := json.Unmarshal(first.Body.Bytes(), &firstBuild); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondBuild); err != nil {
		t.Fatal(err)
	}
	if firstBuild.ID != secondBuild.ID || firstBuild.HostMoref != target.HostMoref ||
		firstBuild.ComputeResourceMoref != target.ComputeResourceMoref {
		t.Fatalf("idempotent/exact target mismatch: first=%+v second=%+v", firstBuild, secondBuild)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM audit_log WHERE user_id = $1`, userID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM template_source_replica_builds WHERE template_id = $1`, templateID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM jobs WHERE type = 'template_replica_build' AND payload->>'template_id' = $1`, templateID.String())
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM template_source_replicas WHERE template_id = $1`, templateID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM template_source_replica_policies WHERE template_id = $1`, templateID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM templates WHERE id = $1`, templateID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM users WHERE id = $1`, userID)
	})
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
