package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/freeradius-manager/backend/internal/auth"
	"github.com/freeradius-manager/backend/internal/database"
	"github.com/freeradius-manager/backend/internal/handlers"
	"github.com/freeradius-manager/backend/internal/middleware"
	"github.com/freeradius-manager/backend/internal/settings"
	"github.com/freeradius-manager/backend/pkg/logger"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

// setupIPRateLimit caps calls to /setup per client IP for the life of the process (a brake
// on guessing SETUP_TOKEN, not a per-minute limit).
var (
	setupIPHits  = map[string]int{}
	setupIPMu    sync.Mutex
	maxSetupHits = 30
)

// weakSecrets are values that have shipped as examples or defaults in this repository.
var weakSecrets = map[string]bool{
	"testing123": true, "ChangeMe_Secure_Pass123!": true, "CHANGE_ME_STRONG_DB_PASSWORD": true,
	"CHANGE_ME_STRONG_RADIUS_SECRET": true, "changeme": true, "password": true,
	"testing123-please-change-to-32-char-minimum-in-production": true,
}

func weakSecret(s string) bool {
	return s == "" || weakSecrets[s] || strings.HasPrefix(strings.ToUpper(s), "CHANGE_ME")
}

// checkSecrets returns the problems with the configured secrets.
func checkSecrets() []string {
	var problems []string
	if err := auth.CheckJWTSecret(); err != nil {
		problems = append(problems, err.Error())
	}
	if weakSecret(os.Getenv("DB_PASSWORD")) {
		problems = append(problems, "DB_PASSWORD is empty or a published example value")
	}
	if r := os.Getenv("RADIUS_SECRET"); weakSecret(r) || len(r) < 12 {
		problems = append(problems, "RADIUS_SECRET is empty, a published example value, or shorter than 12 characters")
	}
	return problems
}

// runCommand runs a maintenance command and returns the process exit code.
func runCommand(db *database.DB, name string) int {
	switch name {
	case "reset-password", "reset-mfa":
		if len(os.Args) < 3 {
			fmt.Fprintf(os.Stderr, "usage: radius-manager %s <staff username>\n", name)
			return 64
		}
		return resetStaffAccount(db, name, os.Args[2])
	case "access-report":
		if _, err := handlers.AccessReport(db.DB, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "access-report:", err)
			return 1
		}
		return 0
	case "resync-access":
		_, failed, err := handlers.ResyncAllAccess(db.DB, os.Stdout)
		if err != nil {
			fmt.Fprintln(os.Stderr, "resync-access:", err)
			return 1
		}
		if failed > 0 {
			return 2
		}
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown command %q\ncommands:\n"+
		"  access-report             list customers who will be refused once access rows are synced\n"+
		"  resync-access             write RADIUS enforcement rows (expiry, status, device limit) for every customer\n"+
		"  reset-password <username> set a staff password (read from stdin), unlock and re-enable the account\n"+
		"  reset-mfa <username>      turn off two-factor authentication for a staff account\n", name)
	return 64
}

// resetStaffAccount is the way back in when the only super admin is locked out or has lost
// their MFA device: run it on the server, e.g.
//
//	docker exec -it radius_backend ./radius-manager reset-password admin
//
// Either way the account is signed out everywhere.
func resetStaffAccount(db *database.DB, cmd, username string) int {
	var id int
	if err := db.QueryRow(`SELECT id FROM app_users WHERE username = $1`, username).Scan(&id); err != nil {
		fmt.Fprintf(os.Stderr, "no staff account named %q\n", username)
		return 1
	}
	if cmd == "reset-mfa" {
		if _, err := db.Exec(`UPDATE app_users SET mfa_enabled = FALSE, mfa_secret = NULL, updated_at = NOW() WHERE id = $1`, id); err != nil {
			fmt.Fprintln(os.Stderr, "reset-mfa:", err)
			return 1
		}
		auth.RevokeAllUserSessions(db.DB, id)
		fmt.Printf("Two-factor authentication turned off for %s; they can set it up again on their Profile page.\n", username)
		return 0
	}

	fmt.Fprint(os.Stderr, "New password: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(os.Stderr, "\nno password given (pass it on stdin)")
		return 1
	}
	password := strings.TrimRight(line, "\r\n")
	if err := auth.ValidatePasswordWithMin(password, auth.MinPasswordLength()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hash:", err)
		return 1
	}
	if _, err := db.Exec(`UPDATE app_users SET password_hash = $1, is_active = TRUE, failed_attempts = 0,
		locked_until = NULL, updated_at = NOW() WHERE id = $2`, hash, id); err != nil {
		fmt.Fprintln(os.Stderr, "reset-password:", err)
		return 1
	}
	auth.RevokeAllUserSessions(db.DB, id)
	fmt.Printf("Password set for %s; the account is unlocked and active.\n", username)
	return 0
}

func main() {
	// Load .env file if present (development mode)
	_ = godotenv.Load()

	log := logger.New()

	// Refuse to start with missing or published secrets: anyone who knows the JWT key can
	// sign in as any admin. APP_ENV=development downgrades this to a warning.
	appEnv := os.Getenv("APP_ENV")
	if problems := checkSecrets(); len(problems) > 0 {
		for _, p := range problems {
			log.Error("insecure configuration: " + p)
		}
		if appEnv != "development" {
			log.Fatal("refusing to start; set real secrets in .env (setup.sh generates them) or APP_ENV=development for local testing")
		}
	}

	// Initialize database
	db, err := database.New()
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()
	settings.Init(db.DB)
	if msg := db.ClockMismatch(); msg != "" {
		log.Error("clock: " + msg)
	}

	// Maintenance commands (run once and exit), e.g.
	//   docker exec radius_backend ./radius-manager access-report
	if len(os.Args) > 1 {
		os.Exit(runCommand(db, os.Args[1]))
	}
	log.Info("Database connection established")

	if appEnv == "production" {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.New()
	// Only these proxies may set X-Forwarded-For. Gin walks the header from the right and
	// stops at the first address that is not a trusted proxy, so a client cannot spoof the
	// IP used for rate limiting, lockouts, IP blocks and the audit log. The default covers
	// loopback and Docker's private ranges (the bundled nginx); list your own load balancer
	// in TRUSTED_PROXIES if there is one in front of nginx.
	trusted := []string{"127.0.0.1/32", "::1/128", "172.16.0.0/12"}
	if v := strings.TrimSpace(os.Getenv("TRUSTED_PROXIES")); v != "" {
		trusted = strings.Split(strings.ReplaceAll(v, " ", ""), ",")
	}
	if err := router.SetTrustedProxies(trusted); err != nil {
		log.Fatalf("TRUSTED_PROXIES: %v", err)
	}
	router.Use(gin.Recovery())
	router.Use(logger.GinLogger(log))
	// Bodies over 20 MB are refused (the CSV import is the largest legitimate upload).
	router.Use(func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 20<<20)
		c.Next()
	})
	router.Use(middleware.RejectUnstorableText())

	// CORS is off by default: the UI is served by nginx on the same origin as /api. Set
	// CORS_ORIGINS (comma-separated, e.g. https://admin.example.com) only if a different
	// site must call the API from a browser.
	if origins := strings.TrimSpace(os.Getenv("CORS_ORIGINS")); origins != "" {
		router.Use(cors.New(cors.Config{
			AllowOrigins:     strings.Split(strings.ReplaceAll(origins, " ", ""), ","),
			AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
			AllowHeaders:     []string{"Origin", "Content-Type", "Authorization", "X-Requested-With", "X-Portal-Token", "Idempotency-Key"},
			ExposeHeaders:    []string{"Content-Length", "X-Request-ID"},
			AllowCredentials: false,
			MaxAge:           12 * time.Hour,
		}))
	}

	// Rate limiter middleware
	rateLimiter := middleware.NewRateLimiter()
	router.Use(rateLimiter.Middleware())

	// Health check (no auth required) — available at both paths so nginx proxy
	// can forward /api/v1/health and the Docker health-check can hit /health.
	healthHandler := handlers.HealthCheck(db)
	router.GET("/health", healthHandler)
	router.GET("/api/v1/health", healthHandler)
	router.GET("/api/v1/version", handlers.Version())

	// Initialize handlers
	h := handlers.New(db, log)

	// ── Background workers ─────────────────────────────────────────────────
	// They run inside the API process. With more than one backend copy, set RUN_WORKERS=false
	// on all but one (scheduled tasks are also protected by a database lock). Note the
	// login rate limiter and lockout windows are per process.
	if os.Getenv("RUN_WORKERS") != "false" {
		handlers.StartNASMonitor(db, log)
		handlers.StartAlertWorker(db, log)
		handlers.StartScheduler(db, log)
		handlers.StartHoneypot(db, log)
		handlers.StartCredStuffingDetector(db, log)
		handlers.StartMpesaLoop(db, log)
	} else {
		log.Info("RUN_WORKERS=false: background workers not started in this process")
	}

	// API v1 routes
	v1 := router.Group("/api/v1")
	{
		// First-run setup wizard. /status always answers (the UI uses it to decide whether
		// to show the wizard); /complete works only while no staff account exists
		// (handlers.SetupRequired) and, if SETUP_TOKEN is set, with that token.
		setup := v1.Group("/setup")
		setup.Use(func(c *gin.Context) {
			if c.Request.Method == http.MethodPost {
				ip := c.ClientIP()
				setupIPMu.Lock()
				setupIPHits[ip]++
				hits := setupIPHits[ip]
				setupIPMu.Unlock()
				if hits > maxSetupHits {
					c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "too many requests"})
					return
				}
			}
			c.Next()
		})
		{
			setup.GET("/status", h.SetupStatus)
			setup.POST("/complete", h.SetupComplete)
		}

		// Auth endpoints (public)
		auth := v1.Group("/auth")
		{
			auth.POST("/login", h.Login)
			auth.POST("/refresh", h.RefreshToken)
			auth.POST("/logout", middleware.RequireAuth(db), h.Logout)
			auth.GET("/profile", middleware.RequireAuth(db), h.GetProfile)
			auth.PUT("/profile", middleware.RequireAuth(db), h.UpdateProfile)
			auth.POST("/change-password", middleware.RequireAuth(db), h.ChangePassword)
			auth.POST("/mfa/setup", middleware.RequireAuth(db), h.MFASetup)
			auth.POST("/mfa/verify", middleware.RequireAuth(db), h.MFAVerify)
		}

		// ── Tier 4 Pro: User Self-Service Portal (public, no JWT) ────────────
		// Customers (radius_users), not admins. Auth is a separate opaque token sent as
		// X-Portal-Token and kept in portal_sessions; only the global per-IP rate limit applies.
		portal := v1.Group("/portal")
		{
			portal.POST("/login", h.PortalLogin)
			portal.POST("/logout", h.PortalLogout)
			portal.GET("/dashboard", h.PortalAuthMiddleware(), h.PortalDashboard)
			portal.POST("/pay", h.PortalAuthMiddleware(), h.PortalPay)
			portal.GET("/pay/:id", h.PortalAuthMiddleware(), h.PortalPayStatus)
		}

		// ── M-Pesa callbacks from Safaricom (public; the :token is MPESA_CALLBACK_TOKEN) ──
		// Daraja refuses URLs containing "mpesa", "safaricom", "query", "sql", "exe" or "cmd"
		// in any case, hence /pay/.
		v1.POST("/pay/result/:token", h.MpesaSTKCallback)
		v1.POST("/pay/validate/:token", h.MpesaC2BValidation)
		v1.POST("/pay/confirm/:token", h.MpesaC2BConfirmation)

		// ── Captive Portal page (public HTML, no JWT) ────────────────────────
		// The page's form posts to the router's hotspot login (captive_portals.login_url or
		// ?link-login-only=), and the router authenticates against FreeRADIUS.
		v1.GET("/captive/serve/:id", h.ServeCaptivePortal)

		// Protected routes: every route below needs a valid session (RequireAuth), and
		// AuditLogger records each POST/PUT/PATCH/DELETE (reads are not audited). The
		// setup, auth, portal and captive routes above sit outside this group, so they
		// are not audited. Per-route RequireRole is the real authorisation check; the
		// frontend's role metadata only hides menu items.
		protected := v1.Group("/")
		protected.Use(middleware.RequireAuth(db))
		protected.Use(middleware.AuditLogger(db, log))

		// Dashboard & Statistics
		protected.GET("statistics/dashboard", middleware.RequireRole("operator", "admin", "super_admin"), h.DashboardStats)
		protected.GET("sessions/active", middleware.RequireRole("operator", "admin", "super_admin"), h.ActiveSessions)
		protected.GET("sessions/user/:username", middleware.RequireRole("operator", "admin", "super_admin"), h.UserSessions)
		protected.GET("logs/auth", middleware.RequireRole("operator", "admin", "super_admin"), h.AuthLogs)
		protected.GET("logs/audit", middleware.RequireRole("admin", "super_admin"), h.AuditLogs)

		// Admin user management (super_admin only)
		adminUsers := protected.Group("admin/users")
		adminUsers.Use(middleware.RequireRole("super_admin"))
		{
			adminUsers.GET("", h.ListAdminUsers)
			adminUsers.POST("", h.CreateAdminUser)
			adminUsers.PUT("/:id", h.UpdateAdminUser)
			adminUsers.DELETE("/:id", h.DeleteAdminUser)
		}

		// RADIUS user management
		radiusUsers := protected.Group("radius/users")
		{
			radiusUsers.GET("", middleware.RequireRole("operator", "admin", "super_admin"), h.ListRadiusUsers)
			radiusUsers.POST("", middleware.RequireRole("admin", "super_admin"), h.CreateRadiusUser)
			radiusUsers.GET("/:id", middleware.RequireRole("operator", "admin", "super_admin"), h.GetRadiusUser)
			radiusUsers.PUT("/:id", middleware.RequireRole("admin", "super_admin"), h.UpdateRadiusUser)
			radiusUsers.DELETE("/:id", middleware.RequireRole("admin", "super_admin"), h.DeleteRadiusUser)
			radiusUsers.POST("/:id/reset-password", middleware.RequireRole("operator", "admin", "super_admin"), h.ResetRadiusUserPassword)
			radiusUsers.POST("/:id/suspend", middleware.RequireRole("admin", "super_admin"), h.SuspendRadiusUser)
			radiusUsers.POST("/:id/activate", middleware.RequireRole("admin", "super_admin"), h.ActivateRadiusUser)
			radiusUsers.POST("/:id/disconnect", middleware.RequireRole("admin", "super_admin"), h.DisconnectRadiusUser)
			radiusUsers.GET("/:id/sessions", middleware.RequireRole("operator", "admin", "super_admin"), h.RadiusUserSessions)
		}

		// Bulk operations
		protected.POST("radius/users/import", middleware.RequireRole("admin", "super_admin"), h.ImportRadiusUsers)
		protected.GET("radius/users/export", middleware.RequireRole("admin", "super_admin"), h.ExportRadiusUsers)

		// NAS device management
		nas := protected.Group("nas")
		{
			nas.GET("", middleware.RequireRole("operator", "admin", "super_admin"), h.ListNAS)
			nas.POST("", middleware.RequireRole("admin", "super_admin"), h.CreateNAS)
			nas.GET("/:id", middleware.RequireRole("operator", "admin", "super_admin"), h.GetNAS)
			nas.PUT("/:id", middleware.RequireRole("admin", "super_admin"), h.UpdateNAS)
			nas.DELETE("/:id", middleware.RequireRole("admin", "super_admin"), h.DeleteNAS)
			nas.POST("/:id/test", middleware.RequireRole("admin", "super_admin"), h.TestNAS)
			nas.POST("/discover", middleware.RequireRole("admin", "super_admin"), h.DiscoverNAS)
		}

		// Network scanner (nmap)
		network := protected.Group("network")
		{
			network.GET("/scanner/status", middleware.RequireRole("admin", "super_admin"), h.NetworkScannerStatus)
			network.POST("/scan", middleware.RequireRole("admin", "super_admin"), h.StartNetworkScan)
			network.GET("/scans", middleware.RequireRole("admin", "super_admin"), h.ListNetworkScans)
			network.GET("/scans/:id", middleware.RequireRole("admin", "super_admin"), h.GetNetworkScan)
			network.DELETE("/scans/:id", middleware.RequireRole("admin", "super_admin"), h.DeleteNetworkScan)
			network.POST("/scans/hosts/:hostId/import-nas", middleware.RequireRole("admin", "super_admin"), h.ImportScanHostAsNAS)
		}

		// RADIUS test endpoint
		protected.POST("radius/test", middleware.RequireRole("admin", "super_admin"), h.TestRADIUS)

		// System settings (super_admin only)
		system := protected.Group("settings")
		{
			system.GET("", middleware.RequireRole("super_admin"), h.GetSettings)
			system.PUT("", middleware.RequireRole("super_admin"), h.UpdateSettings)
		}

		// Backups (super_admin only): pg_dump to BACKUP_DIR, also run daily by the scheduler.
		// Restore is a manual step on the server (POST /restore only returns instructions).
		protected.POST("backup", middleware.RequireRole("super_admin"), h.CreateBackup)
		protected.POST("restore", middleware.RequireRole("super_admin"), h.RestoreBackup)
		protected.GET("backups", middleware.RequireRole("super_admin"), h.ListBackups)
		protected.GET("backups/:name", middleware.RequireRole("super_admin"), h.DownloadBackup)
		protected.DELETE("backups/:name", middleware.RequireRole("super_admin"), h.DeleteBackup)

		// ── Tier 1 Pro: Vouchers ─────────────────────────────────────────────
		vouchers := protected.Group("vouchers")
		{
			vouchers.GET("", middleware.RequireRole("operator", "admin", "super_admin"), h.ListVouchers)
			vouchers.POST("/generate", middleware.RequireRole("admin", "super_admin"), h.GenerateVouchers)
			vouchers.GET("/batches", middleware.RequireRole("operator", "admin", "super_admin"), h.GetVoucherBatches)
			vouchers.POST("/:id/disable", middleware.RequireRole("admin", "super_admin"), h.DisableVoucher)
			vouchers.DELETE("/:id", middleware.RequireRole("admin", "super_admin"), h.DeleteVoucher)
			vouchers.GET("/export", middleware.RequireRole("admin", "super_admin"), h.ExportVouchers)
		}

		// ── Tier 1 Pro: Bandwidth Profiles ───────────────────────────────────
		bw := protected.Group("bandwidth-profiles")
		{
			bw.GET("", middleware.RequireRole("operator", "admin", "super_admin"), h.ListBandwidthProfiles)
			bw.POST("", middleware.RequireRole("admin", "super_admin"), h.CreateBandwidthProfile)
			bw.PUT("/:id", middleware.RequireRole("admin", "super_admin"), h.UpdateBandwidthProfile)
			bw.DELETE("/:id", middleware.RequireRole("admin", "super_admin"), h.DeleteBandwidthProfile)
		}
		// Apply bandwidth profile to a RADIUS user
		protected.POST("radius/users/:id/bandwidth", middleware.RequireRole("admin", "super_admin"), h.ApplyBandwidthProfile)

		// ── Tier 1 Pro: Reports ───────────────────────────────────────────────
		reports := protected.Group("reports")
		reports.Use(middleware.RequireRole("operator", "admin", "super_admin"))
		{
			reports.GET("/usage", h.UsageReport)
			reports.GET("/usage/daily", h.DailyUsageReport)
			reports.GET("/auth", h.AuthSuccessReport)
			reports.GET("/nas", h.NASUsageReport)
			reports.GET("/usage/export", h.ExportUsageReport)
		}

		// ── Tier 2 Pro: User Plans ────────────────────────────────────────────
		plans := protected.Group("plans")
		{
			plans.GET("", middleware.RequireRole("operator", "admin", "super_admin"), h.ListPlans)
			plans.POST("", middleware.RequireRole("admin", "super_admin"), h.CreatePlan)
			plans.PUT("/:id", middleware.RequireRole("admin", "super_admin"), h.UpdatePlan)
			plans.DELETE("/:id", middleware.RequireRole("admin", "super_admin"), h.DeletePlan)
		}
		protected.POST("radius/users/:id/plan", middleware.RequireRole("admin", "super_admin"), h.AssignPlan)

		// ── Tier 2 Pro: Billing / Invoices ────────────────────────────────────
		billing := protected.Group("invoices")
		{
			billing.GET("", middleware.RequireRole("operator", "admin", "super_admin"), h.ListInvoices)
			billing.GET("/:id", middleware.RequireRole("operator", "admin", "super_admin"), h.GetInvoice)
			billing.POST("", middleware.RequireRole("admin", "super_admin"), h.CreateInvoice)
			billing.PUT("/:id", middleware.RequireRole("admin", "super_admin"), h.UpdateInvoice)
			billing.DELETE("/:id", middleware.RequireRole("admin", "super_admin"), h.DeleteInvoice)
		}

		// ── Tier 2 Pro: NAS Monitor ───────────────────────────────────────────
		protected.GET("nas/status", middleware.RequireRole("operator", "admin", "super_admin"), h.GetNASStatus)
		protected.POST("nas/:id/ping", middleware.RequireRole("admin", "super_admin"), h.PingNASNow)

		// ── Tier 2 Pro: Alert Rules ───────────────────────────────────────────
		alerts := protected.Group("alerts")
		{
			alerts.GET("", middleware.RequireRole("admin", "super_admin"), h.ListAlertRules)
			alerts.POST("", middleware.RequireRole("super_admin"), h.CreateAlertRule)
			alerts.PUT("/:id", middleware.RequireRole("super_admin"), h.UpdateAlertRule)
			alerts.DELETE("/:id", middleware.RequireRole("super_admin"), h.DeleteAlertRule)
			alerts.POST("/test-email", middleware.RequireRole("super_admin"), h.SendTestEmail)
		}

		// ── Tier 3 Pro: IP Pools ──────────────────────────────────────────────
		pools := protected.Group("ip-pools")
		{
			pools.GET("", middleware.RequireRole("operator", "admin", "super_admin"), h.ListIPPools)
			pools.POST("", middleware.RequireRole("admin", "super_admin"), h.CreateIPPool)
			pools.DELETE("/:id", middleware.RequireRole("admin", "super_admin"), h.DeleteIPPool)
			pools.GET("/:id/ips", middleware.RequireRole("operator", "admin", "super_admin"), h.ListPoolIPs)
		}
		protected.POST("ip-pools/assign", middleware.RequireRole("admin", "super_admin"), h.AssignIP)
		protected.POST("ip-pools/release", middleware.RequireRole("admin", "super_admin"), h.ReleaseIP)

		// ── API Keys ──────────────────────────────────────────────────────────
		// Keys authenticate as "Authorization: ApiKey <key>" (middleware.RequireAuth):
		// read keys = operator, GET only; write keys = admin.
		apikeys := protected.Group("api-keys")
		{
			apikeys.GET("", middleware.RequireRole("super_admin"), h.ListAPIKeys)
			apikeys.POST("", middleware.RequireRole("super_admin"), h.CreateAPIKey)
			apikeys.POST("/:id/revoke", middleware.RequireRole("super_admin"), h.RevokeAPIKey)
			apikeys.DELETE("/:id", middleware.RequireRole("super_admin"), h.DeleteAPIKey)
			apikeys.GET("/stats", middleware.RequireRole("admin", "super_admin"), h.APIKeyStats)
		}

		// ── Tier 3 Pro: Scheduler ─────────────────────────────────────────────
		sched := protected.Group("scheduler")
		{
			sched.GET("", middleware.RequireRole("admin", "super_admin"), h.ListScheduledTasks)
			sched.POST("/:id/toggle", middleware.RequireRole("super_admin"), h.ToggleTask)
			sched.POST("/:id/run", middleware.RequireRole("super_admin"), h.RunTaskNow)
			sched.PUT("/:id/schedule", middleware.RequireRole("super_admin"), h.UpdateTaskSchedule)
		}

		// ── Tier 3 Pro: Bulk Import / Export (replaces existing handlers) ────
		// NOTE: routes already registered above as radius/users/import & export

		// ── Tier 4 Pro: Hotspot Zones ─────────────────────────────────────────
		zones := protected.Group("zones")
		{
			zones.GET("", middleware.RequireRole("operator", "admin", "super_admin"), h.ListZones)
			zones.POST("", middleware.RequireRole("admin", "super_admin"), h.CreateZone)
			zones.PUT("/:id", middleware.RequireRole("admin", "super_admin"), h.UpdateZone)
			zones.DELETE("/:id", middleware.RequireRole("admin", "super_admin"), h.DeleteZone)
			zones.GET("/:id/stats", middleware.RequireRole("operator", "admin", "super_admin"), h.ZoneStats)
		}
		protected.POST("zones/assign-nas", middleware.RequireRole("admin", "super_admin"), h.AssignNASToZone)

		// ── Tier 4 Pro: Live Stats SSE ────────────────────────────────────────
		protected.GET("live/stats", middleware.RequireRole("operator", "admin", "super_admin"), h.LiveStats)
		protected.GET("live/stats/current", middleware.RequireRole("operator", "admin", "super_admin"), h.GetCurrentStats)

		// ── Tier 4 Pro: SMS ───────────────────────────────────────────────────
		smsGroup := protected.Group("sms")
		{
			smsGroup.POST("/send", middleware.RequireRole("admin", "super_admin"), h.SendSMS)
			smsGroup.GET("/logs", middleware.RequireRole("admin", "super_admin"), h.ListSMSLogs)
			smsGroup.POST("/notify-expiry", middleware.RequireRole("admin", "super_admin"), h.NotifyUserExpiry)
			smsGroup.GET("/config", middleware.RequireRole("super_admin"), h.SMSConfig)
		}

		// ── Tier 5 Pro: Organizations / Resellers ─────────────────────────────
		orgs := protected.Group("organizations")
		{
			orgs.GET("", middleware.RequireRole("admin", "super_admin"), h.ListOrganizations)
			orgs.POST("", middleware.RequireRole("super_admin"), h.CreateOrganization)
			orgs.PUT("/:id", middleware.RequireRole("super_admin"), h.UpdateOrganization)
			orgs.DELETE("/:id", middleware.RequireRole("super_admin"), h.DeleteOrganization)
			orgs.GET("/:id/stats", middleware.RequireRole("admin", "super_admin"), h.OrgStats)
		}
		protected.POST("organizations/assign-user", middleware.RequireRole("admin", "super_admin"), h.AssignUserToOrg)

		// ── Tier 5 Pro: Customers CRM ─────────────────────────────────────────
		customers := protected.Group("customers")
		{
			customers.GET("", middleware.RequireRole("operator", "admin", "super_admin"), h.ListCustomers)
			customers.GET("/:id", middleware.RequireRole("operator", "admin", "super_admin"), h.GetCustomer)
			customers.POST("", middleware.RequireRole("admin", "super_admin"), h.CreateCustomer)
			customers.PUT("/:id", middleware.RequireRole("admin", "super_admin"), h.UpdateCustomer)
			customers.DELETE("/:id", middleware.RequireRole("admin", "super_admin"), h.DeleteCustomer)
		}

		// ── Tier 5 Pro: Support Tickets ───────────────────────────────────────
		tickets := protected.Group("tickets")
		{
			tickets.GET("", middleware.RequireRole("operator", "admin", "super_admin"), h.ListTickets)
			tickets.POST("", middleware.RequireRole("operator", "admin", "super_admin"), h.CreateTicket)
			tickets.PUT("/:id", middleware.RequireRole("operator", "admin", "super_admin"), h.UpdateTicket)
			tickets.DELETE("/:id", middleware.RequireRole("admin", "super_admin"), h.DeleteTicket)
		}

		// ── Tier 5 Pro: Captive Portals (admin) ───────────────────────────────
		captive := protected.Group("captive")
		{
			captive.GET("", middleware.RequireRole("operator", "admin", "super_admin"), h.ListCaptivePortals)
			captive.POST("", middleware.RequireRole("admin", "super_admin"), h.CreateCaptivePortal)
			captive.PUT("/:id", middleware.RequireRole("admin", "super_admin"), h.UpdateCaptivePortal)
			captive.DELETE("/:id", middleware.RequireRole("admin", "super_admin"), h.DeleteCaptivePortal)
		}

		// ── Tier 5 Pro: Webhooks ─────────────────────────────────────────────
		hooks := protected.Group("webhooks")
		{
			hooks.GET("", middleware.RequireRole("admin", "super_admin"), h.ListWebhooks)
			hooks.POST("", middleware.RequireRole("super_admin"), h.CreateWebhook)
			hooks.PUT("/:id", middleware.RequireRole("super_admin"), h.UpdateWebhook)
			hooks.DELETE("/:id", middleware.RequireRole("super_admin"), h.DeleteWebhook)
			hooks.POST("/:id/test", middleware.RequireRole("super_admin"), h.TestWebhook)
			hooks.GET("/:id/logs", middleware.RequireRole("admin", "super_admin"), h.ListWebhookLogs)
		}

		// ── Tier 6 Pro: Payments ─────────────────────────────────────────────
		mp := protected.Group("mpesa-admin")
		{
			mp.GET("/status", middleware.RequireRole("operator", "admin", "super_admin"), h.MpesaStatus)
			mp.POST("/prompt", middleware.RequireRole("operator", "admin", "super_admin"), h.MpesaSendPrompt)
			mp.GET("/prompt/:id", middleware.RequireRole("operator", "admin", "super_admin"), h.MpesaPromptStatus)
			mp.GET("/prompts", middleware.RequireRole("operator", "admin", "super_admin"), h.MpesaListPrompts)
			mp.GET("/c2b", middleware.RequireRole("operator", "admin", "super_admin"), h.MpesaListC2B)
			mp.POST("/c2b/:id/allocate", middleware.RequireRole("admin", "super_admin"), h.MpesaAllocateC2B)
			mp.POST("/register-c2b", middleware.RequireRole("super_admin"), h.MpesaRegisterC2B)
		}

		payments := protected.Group("payments")
		{
			payments.GET("", middleware.RequireRole("operator", "admin", "super_admin"), h.ListPayments)
			payments.POST("", middleware.RequireRole("admin", "super_admin"), h.CreatePayment)
			payments.POST("/:id/reverse", middleware.RequireRole("admin", "super_admin"), h.ReversePayment)
			payments.DELETE("/:id", middleware.RequireRole("admin", "super_admin"), h.ReversePayment)
			payments.GET("/:id/receipt", middleware.RequireRole("operator", "admin", "super_admin"), h.GetPaymentReceipt)
			payments.GET("/summary", middleware.RequireRole("admin", "super_admin"), h.PaymentSummary)
		}

		// ── Tier 6 Pro: RADIUS Attribute Templates ────────────────────────────
		tmpl := protected.Group("templates")
		{
			tmpl.GET("", middleware.RequireRole("operator", "admin", "super_admin"), h.ListTemplates)
			tmpl.POST("", middleware.RequireRole("admin", "super_admin"), h.CreateTemplate)
			tmpl.PUT("/:id", middleware.RequireRole("admin", "super_admin"), h.UpdateTemplate)
			tmpl.DELETE("/:id", middleware.RequireRole("admin", "super_admin"), h.DeleteTemplate)
			tmpl.POST("/:id/apply", middleware.RequireRole("admin", "super_admin"), h.ApplyTemplate)
			tmpl.POST("/:id/clone", middleware.RequireRole("admin", "super_admin"), h.CloneTemplate)
		}

		// ── Tier 6 Pro: Promotions ────────────────────────────────────────────
		promos := protected.Group("promotions")
		{
			promos.GET("", middleware.RequireRole("operator", "admin", "super_admin"), h.ListPromotions)
			promos.POST("", middleware.RequireRole("admin", "super_admin"), h.CreatePromotion)
			promos.PUT("/:id", middleware.RequireRole("admin", "super_admin"), h.UpdatePromotion)
			promos.DELETE("/:id", middleware.RequireRole("admin", "super_admin"), h.DeletePromotion)
			promos.POST("/validate", middleware.RequireRole("operator", "admin", "super_admin"), h.ValidatePromoCode)
			promos.POST("/apply", middleware.RequireRole("operator", "admin", "super_admin"), h.ApplyPromoCode)
		}

		// ── Tier 6 Pro: Bulk Operations ───────────────────────────────────────
		bulk := protected.Group("bulk")
		{
			bulk.POST("", middleware.RequireRole("admin", "super_admin"), h.BulkOperation)
			bulk.GET("/history", middleware.RequireRole("admin", "super_admin"), h.ListBulkOpHistory)
		}

		// ── Tier 7: Security Suite ────────────────────────────────────────────
		// RADIUS Simulator
		sim := protected.Group("radius/simulate")
		{
			sim.POST("", middleware.RequireRole("admin", "super_admin"), h.SimulateAuth)
			sim.POST("/batch", middleware.RequireRole("admin", "super_admin"), h.SimulateBatch)
		}

		// GeoIP rules and IP blocks apply to the staff and customer-portal logins
		// (handlers.loginGuard), not to RADIUS.
		geoip := protected.Group("security/geoip")
		{
			geoip.GET("/lookup", middleware.RequireRole("operator", "admin", "super_admin"), h.GeoIPLookup)
			geoip.GET("/rules", middleware.RequireRole("operator", "admin", "super_admin"), h.ListGeoIPRules)
			geoip.POST("/rules", middleware.RequireRole("admin", "super_admin"), h.CreateGeoIPRule)
			geoip.DELETE("/rules/:id", middleware.RequireRole("admin", "super_admin"), h.DeleteGeoIPRule)
		}

		// Honeypot
		hp := protected.Group("security/honeypot")
		{
			hp.GET("/status", middleware.RequireRole("operator", "admin", "super_admin"), h.HoneypotStatus)
			hp.GET("/logs", middleware.RequireRole("admin", "super_admin"), h.ListHoneypotLogs)
			hp.DELETE("/logs", middleware.RequireRole("admin", "super_admin"), h.ClearHoneypotLogs)
		}

		// Credential Stuffing + IP blocking
		sec := protected.Group("security")
		{
			sec.GET("/summary", middleware.RequireRole("operator", "admin", "super_admin"), h.SecuritySummary)
			sec.GET("/alerts", middleware.RequireRole("operator", "admin", "super_admin"), h.ListSecurityAlerts)
			sec.PUT("/alerts/:id/ack", middleware.RequireRole("operator", "admin", "super_admin"), h.AcknowledgeAlert)
			sec.PUT("/alerts/ack-all", middleware.RequireRole("admin", "super_admin"), h.AcknowledgeAllAlerts)
			sec.DELETE("/alerts/:id", middleware.RequireRole("admin", "super_admin"), h.DeleteAlert)
			sec.GET("/blocked-ips", middleware.RequireRole("admin", "super_admin"), h.GetBlockedIPs)
			sec.POST("/blocked-ips", middleware.RequireRole("admin", "super_admin"), h.BlockIP)
			sec.DELETE("/blocked-ips/:id", middleware.RequireRole("admin", "super_admin"), h.UnblockIP)
		}
	}

	port := os.Getenv("WEB_PORT")
	if port == "" {
		port = os.Getenv("PORT") // set by hosting platforms such as Render
	}
	if port == "" {
		port = "8080"
	}

	// WriteTimeout bounds normal responses; the live-stats stream and backup downloads lift
	// it for themselves (http.ResponseController).
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%s", port),
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	go func() {
		log.Infof("Backend server starting on port %s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("Shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Errorf("Server forced shutdown: %v", err)
	}
	log.Info("Server stopped")
}
