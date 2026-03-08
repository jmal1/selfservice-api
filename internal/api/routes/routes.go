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
func Setup(h *handlers.Handler, authProvider *auth.Provider, db *database.Queries) *chi.Mux {
	r := chi.NewRouter()

	// Global middleware
	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Recoverer)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"https://crucible.lab.jmal.io"},
		AllowedMethods:   []string{"GET", "POST", "PATCH", "DELETE", "OPTIONS"},
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
		})

		// Templates
		r.Get("/templates", h.ListTemplates)

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

			r.Get("/jobs", h.AdminListJobs)
			r.Get("/audit", h.AdminListAuditLog)
			r.Get("/audit/search", h.AdminSearchAuditLog)

			r.Get("/sessions", h.AdminListSessions)

			r.Get("/vlans", h.AdminListVLANPool)
			r.Post("/vlans", h.AdminAddVLAN)
			r.Patch("/vlans/{vlanID}", h.AdminUpdateVLAN)
			r.Delete("/vlans/{vlanID}", h.AdminRemoveVLAN)
		})
	})
	}) // close r.Group for Logger/Compress

	return r
}
