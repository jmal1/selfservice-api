package routes

import (
	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"

	"github.com/jmal1/selfservice-api/internal/api/handlers"
	"github.com/jmal1/selfservice-api/internal/auth"
	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
)

// Setup creates the chi router with all routes.
func Setup(h *handlers.Handler, authProvider *auth.Provider, db *database.Queries, allowedOrigins []string) *chi.Mux {
	r := chi.NewRouter()

	// Global middleware
	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Recoverer)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   allowedOrigins,
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Content-Type", "Authorization"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	// Health check (unauthenticated)
	r.Get("/healthz", h.Health)
	r.Get("/readyz", h.Health)

	// WebSocket routes — mounted before Logger/Compress which break http.Hijacker
	r.Route("/api/v1/pods/{podID}/vms/{vmID}/console", func(r chi.Router) {
		r.Use(middleware.Auth(authProvider))
		r.Get("/ws", h.VMConsoleWS)
	})

	// Run progress WebSocket — streams live workflow_start/action_complete/
	// workflow_complete events for a given run. Replaces 2s polling.
	r.Route("/api/v1/runs/{runID}", func(r chi.Router) {
		r.Use(middleware.Auth(authProvider))
		r.Get("/progress/ws", h.RunProgressWS)
	})

	// Non-WebSocket routes get Logger + Compress
	r.Group(func(r chi.Router) {
		r.Use(chimiddleware.Logger)
		r.Use(chimiddleware.Compress(5))

	// Auth routes (unauthenticated)
	r.Route("/auth", func(r chi.Router) {
		r.Get("/login", authProvider.LoginHandler)
		r.Get("/callback", authProvider.CallbackHandler)
		r.Post("/logout", authProvider.LogoutHandler)

		// Authenticated
		r.Group(func(r chi.Router) {
			r.Use(middleware.Auth(authProvider))
			r.Get("/me", h.GetMe)
		})
	})

	// API v1 (authenticated)
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(middleware.Auth(authProvider))
		r.Use(middleware.AuditRequests(db))

		// Pods
		r.Route("/pods", func(r chi.Router) {
			r.Get("/", h.ListPods)
			r.Post("/", h.CreatePod)
			r.Get("/{podID}", h.GetPod)
			r.Delete("/{podID}", h.DeletePod)

			// VM sub-routes
			r.Post("/{podID}/vms", h.AddVM)
			r.Delete("/{podID}/vms/{vmID}", h.DeleteVM)

			// VM power operations
			r.Post("/{podID}/vms/{vmID}/start", h.VMPowerAction)
			r.Post("/{podID}/vms/{vmID}/stop", h.VMPowerAction)
			r.Post("/{podID}/vms/{vmID}/restart", h.VMPowerAction)
			r.Post("/{podID}/vms/{vmID}/reset", h.VMPowerAction)

			// VM snapshot operations
			r.Get("/{podID}/vms/{vmID}/snapshots", h.ListVMSnapshots)
			r.Post("/{podID}/vms/{vmID}/snapshots", h.CreateVMSnapshot)
			r.Post("/{podID}/vms/{vmID}/snapshots/revert-initial", h.RevertToInitial)
			r.Post("/{podID}/vms/{vmID}/snapshots/{snapID}/revert", h.RevertToSnapshot)
			r.Delete("/{podID}/vms/{vmID}/snapshots/{snapID}", h.DeleteVMSnapshot)

			// Pod expiration
			r.Post("/{podID}/extend", h.ExtendPod)

			// Testing (assessments)
			r.Route("/{podID}/testing", func(r chi.Router) {
				r.Get("/", h.GetTestingDashboard)
				r.Post("/run", h.CreateTestingRun)
				r.Get("/runs", h.ListTestingRuns)
				r.Get("/runs/{runID}", h.GetTestingRun)
				r.Post("/runs/{runID}/cancel", h.CancelTestingRun)
			})
		})

		// Templates
		r.Get("/templates", h.ListTemplates)

		// Blueprints
		r.Route("/blueprints", func(r chi.Router) {
			r.Get("/", h.ListBlueprints)
			r.Get("/{blueprintID}", h.GetBlueprint)
			r.Post("/{blueprintID}/deploy", h.DeployBlueprint)
		})

		// Jobs
		r.Get("/jobs", h.ListMyJobs)
		r.Get("/jobs/{jobID}/status", h.GetJobStatus)

		// Admin routes
		r.Route("/admin", func(r chi.Router) {
			r.Use(middleware.RequireRole(models.RoleAdmin))

			r.Get("/users", h.AdminListUsers)
			r.Patch("/users/{userID}/quotas", h.AdminUpdateQuotas)

			r.Get("/templates", h.AdminListTemplates)
			r.Post("/templates", h.AdminCreateTemplate)
			r.Patch("/templates/{templateID}", h.AdminUpdateTemplate)
			r.Delete("/templates/{templateID}", h.AdminDeleteTemplate)
			r.Post("/templates/{templateID}/access", h.AdminSetTemplateAccess)
			r.Get("/templates/{templateID}/dependents", h.AdminListTemplateDependents)

			// vCenter folder browser for template registration UI (cached 5 min).
			r.Get("/vcenter/templates-folder", h.AdminListVCenterTemplatesFolder)

			r.Get("/jobs", h.AdminListJobs)
			r.Get("/audit", h.AdminListAuditLog)
			r.Get("/audit/search", h.AdminSearchAuditLog)

			r.Get("/sessions", h.AdminListSessions)

			r.Get("/vlans", h.AdminListVLANPool)
			r.Post("/vlans", h.AdminAddVLAN)
			r.Patch("/vlans/{vlanID}", h.AdminUpdateVLAN)
			r.Delete("/vlans/{vlanID}", h.AdminRemoveVLAN)

			// Blueprints
			r.Get("/blueprints", h.AdminListBlueprints)
			r.Post("/blueprints", h.AdminCreateBlueprint)
			r.Put("/blueprints/{blueprintID}", h.AdminUpdateBlueprint)
			r.Delete("/blueprints/{blueprintID}", h.AdminDeleteBlueprint)
			r.Post("/blueprints/{blueprintID}/access", h.AdminSetBlueprintAccess)

			// Admin pod management
			r.Post("/pods/{podID}/extend", h.AdminExtendPod)

			// Workflows (assessment scripts)
			r.Route("/workflows", func(r chi.Router) {
				r.Get("/", h.AdminListWorkflows)
				r.Post("/", h.AdminCreateWorkflow)
				r.Post("/import", h.AdminImportWorkflows)
				r.Get("/export", h.AdminExportWorkflows)
				r.Get("/{workflowID}", h.AdminGetWorkflow)
				r.Put("/{workflowID}", h.AdminUpdateWorkflow)
				r.Delete("/{workflowID}", h.AdminDeleteWorkflow)
				r.Post("/{workflowID}/submit", h.AdminSubmitWorkflow)
				r.Post("/{workflowID}/approve", h.AdminApproveWorkflow)
				r.Post("/{workflowID}/activate", h.AdminActivateWorkflow)
			})

			// Actions (reusable action library)
			r.Route("/actions", func(r chi.Router) {
				r.Get("/", h.AdminListActions)
				r.Post("/", h.AdminCreateAction)
				r.Get("/{actionID}", h.AdminGetAction)
				r.Put("/{actionID}", h.AdminUpdateAction)
				r.Delete("/{actionID}", h.AdminDeleteAction)
			})

			// Script validator (shellcheck-backed) — used by the workflow
			// + action editor to surface lint findings as Monaco markers.
			r.Post("/scripts/validate", h.AdminValidateScript)

			// Playlists
			r.Route("/playlists", func(r chi.Router) {
				r.Get("/", h.AdminListPlaylists)
				r.Post("/", h.AdminCreatePlaylist)
				r.Get("/{playlistID}", h.AdminGetPlaylist)
				r.Put("/{playlistID}", h.AdminUpdatePlaylist)
				r.Delete("/{playlistID}", h.AdminDeletePlaylist)
			})

			// Template playlist assignment
			r.Get("/templates/{templateID}/playlists", h.AdminGetTemplatePlaylists)
			r.Post("/templates/{templateID}/playlists", h.AdminSetTemplatePlaylists)

			// Blueprint VM playlist overrides
			r.Post("/blueprints/{blueprintID}/vm-playlists", h.AdminSetBlueprintVMPlaylists)

			// Testing runs (admin view)
			r.Get("/runs", h.AdminListRuns)
		})
	})
	}) // close r.Group for Logger/Compress

	return r
}
