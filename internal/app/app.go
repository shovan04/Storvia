// Package app wires the Storvia service together.
//
// Every constructor here is explicit about what it needs and what it refuses to
// do without. In particular a missing credential set degrades the relevant
// provider to a reporting-only state rather than failing the process, because a
// container that refuses to start cannot report why it is unready.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"time"

	"github.com/shovan04/storvia-project/internal/auth"
	"github.com/shovan04/storvia-project/internal/cache"
	"github.com/shovan04/storvia-project/internal/config"
	"github.com/shovan04/storvia-project/internal/httpapi"
	"github.com/shovan04/storvia-project/internal/idid"
	"github.com/shovan04/storvia-project/internal/metrics"
	"github.com/shovan04/storvia-project/internal/persistence"
	"github.com/shovan04/storvia-project/internal/secrets"
	"github.com/shovan04/storvia-project/internal/storage/provider/drive"
	"github.com/shovan04/storvia-project/internal/storage/provider/s3compat"
	"github.com/shovan04/storvia-project/internal/storage/service"
)

// App is the assembled service.
type App struct {
	Config *config.Config
	Logger *slog.Logger
	Store  *persistence.Store
	Cache  cache.Cache
	Server *http.Server

	// Backends holds the constructed S3-compatible clients, keyed by backend ID.
	// It is empty when fallback is disabled.
	Backends map[string]*s3compat.Client
	// secrets holds resolved secret values keyed by environment variable name.
	// It never leaves this struct and is never logged.
	secrets map[string]string
	// Uploads and Objects are the use cases, exposed so tests can drive them
	// without going through HTTP.
	Uploads *service.UploadService
	Objects *service.ObjectService
	// Consent exposes the tenant's fallback-consent record and the endpoint that
	// changes it.
	Consent *service.ConsentService
	// DriveFaultsEnabled reports whether deliberate Drive fault injection is armed.
	// The admin panel shows it, because a service that believes Drive is down when
	// it is not would otherwise be very confusing to debug.
	DriveFaultsEnabled bool
	// WorkerStop ends the background worker and waits for it. It is non-nil only
	// when a worker was started.
	WorkerStop func()
	// PollerStop ends the Drive change-log poller.
	PollerStop func()
	// RoutingSwitch is the live Drive-vs-fallback control.
	RoutingSwitch *RoutingSwitch
	// bgCancel stops the background worker and the change poller.
	//
	// The background components take their own cancellable context rather than the
	// caller's. Waiting for them to finish is only correct if something has actually
	// cancelled them: stopping them relied on the process's signal handler having run
	// first, which meant a clean stop from anywhere else -- a test, a library caller,
	// an in-process restart -- waited forever.
	bgCancel context.CancelFunc
	// Connections is the Drive connection lifecycle service.
	Connections *service.ConnectionService
	// Drive is the Google Drive provider client.
	Drive *drive.Client
	// Issuer and Sessions are the session machinery, exposed so tests can mint a
	// real session and drive the authenticated console routes rather than a
	// hand-built one that would not exercise the actual cookie handling.
	Issuer   *auth.Issuer
	Sessions auth.Lookup
	// Issues lists degraded-but-started conditions an operator must resolve.
	Issues []string
}

// Options are the inputs to Build.
type Options struct {
	Config *config.Config
	Logger *slog.Logger
}

// Build assembles the service.
//
// It returns an App whose Issues describe anything degraded, plus an error only
// for conditions that make the service unsafe to run at all.
func Build(ctx context.Context, opts Options) (*App, error) {
	// Build information is recorded before anything else so a scrape taken during a
	// failed start still identifies the process.
	buildLabels := map[string]string{
		"version":     releaseVersion(opts.Config),
		"go":          runtime.Version(),
		"pid":         strconv.Itoa(os.Getpid()),
		"environment": opts.Config.Environment,
	}
	appMetrics := metrics.NewStorvia(buildLabels)

	cfg := opts.Config
	if cfg == nil {
		return nil, errors.New("app: configuration is required")
	}

	// The routing switch lets an operator send uploads to the fallback provider from
	// the panel without editing configuration and restarting. It is created before the
	// services so they can hold a reference to it.
	routingSwitch := NewRoutingSwitch(cfg.Fallback.Force)
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	app := &App{
		Config:        cfg,
		Logger:        logger,
		Backends:      make(map[string]*s3compat.Client),
		RoutingSwitch: routingSwitch,
	}

	// Every secret is resolved once, here, and fails fast if a required one is
	// absent. Previously each call site read the environment independently, so a
	// missing secret surfaced much later as an authentication failure, or as a
	// fallback backend that was silently skipped while appearing configured.
	resolvedSecrets, err := cfg.ResolveSecrets(os.LookupEnv)
	if err != nil {
		return nil, err
	}
	app.secrets = resolvedSecrets

	store, err := persistence.Open(ctx, cfg.Database, resolvedSecrets[config.EnvDatabasePassword])
	if err != nil {
		return nil, err
	}
	app.Store = store

	if cfg.Database.AutoMigrate {
		applied, err := store.Migrate(ctx)
		if err != nil {
			store.Close()
			return nil, err
		}
		if applied > 0 {
			logger.InfoContext(ctx, "applied database migrations", "count", applied)
		}
	}

	cacheBackend, err := buildCache(ctx, cfg.Cache, resolvedSecrets)
	if err != nil {
		store.Close()
		return nil, err
	}
	app.Cache = cacheBackend

	backends, issues := buildBackends(ctx, cfg, resolvedSecrets, logger)
	app.Backends = backends
	app.Issues = append(app.Issues, issues...)

	keyring, err := buildKeyring(cfg, resolvedSecrets)
	if err != nil {
		// Without a keyring no refresh token can be stored safely, so this is a
		// hard stop rather than a degraded mode.
		cacheBackend.Close()
		store.Close()
		return nil, err
	}

	driveClient, err := buildDrive(ctx, cfg, store, resolvedSecrets, logger, keyring)
	if err != nil {
		cacheBackend.Close()
		store.Close()
		return nil, err
	}
	if !driveClient.Configured() {
		app.Issues = append(app.Issues,
			"google drive credentials are absent: drive uploads and reads are unavailable until they are set")
	}

	uploads, objects, connections, consent, err := buildServices(cfg, store, keyring, driveClient, backends, logger, appMetrics, routingSwitch)
	if err != nil {
		cacheBackend.Close()
		store.Close()
		return nil, err
	}
	app.Uploads = uploads
	app.Objects = objects
	app.Consent = consent

	issuer, sessions, err := buildAuth(ctx, cfg, resolvedSecrets, cacheBackend)
	if err != nil {
		cacheBackend.Close()
		store.Close()
		return nil, err
	}

	adminCfg := httpapi.AdminPanelConfig{
		Enabled:         cfg.AdminPanel.Enabled,
		TenantID:        cfg.AdminPanel.TenantID,
		UserID:          cfg.AdminPanel.UserID,
		FlowCookie:      cfg.AdminPanel.FlowCookie,
		RedirectURI:     cfg.Drive.AdminRedirectURI,
		Secure:          cfg.Auth.SecureCookie,
		SessionTTL:      cfg.Auth.SessionTTL.Duration(),
		AdminSessionTTL: cfg.Auth.AdminSessionTTL.Duration(),
		ConsoleURL:      cfg.AdminPanel.ConsoleURL,
	}
	// A process override, so the development launcher can point Go at a frontend on a
	// different port without editing checked-in configuration. Read straight from the
	// environment rather than through ResolveSecrets: that resolves credentials, and this
	// is a location, not a secret.
	if override, ok := os.LookupEnv(config.EnvConsoleURL); ok && override != "" {
		adminCfg.ConsoleURL = override
	}
	// The development tenant named by configuration must exist before anything can be
	// written for it. Consent, Drive connections, and objects all reference the
	// tenant, so without this row the panel appears to work right up until the first
	// write, which is the least useful moment to discover it.
	if cfg.AdminPanel.Enabled && cfg.AdminPanel.TenantID != "" {
		if err := persistence.NewTenantRepository(store).EnsureTenant(
			context.Background(), cfg.AdminPanel.TenantID, "Storvia development tenant"); err != nil {
			cacheBackend.Close()
			store.Close()
			return nil, fmt.Errorf("app: development tenant: %w", err)
		}
	}

	// The console reports what the deployment can actually do, which requires the
	// built services rather than the configuration that produced them.
	adminScope := persistence.Scope{TenantID: cfg.AdminPanel.TenantID, UserID: cfg.AdminPanel.UserID}
	// The fault switch drives the same injector the drive client holds, so toggling
	// it in the console takes effect on the next upload rather than at the next start.
	faults := httpapi.NewFaultSwitch(driveClient.FaultsEnabled(), faultToggle{client: driveClient})

	console := httpapi.NewConsole(adminCfg, httpapi.ConsoleOptions{
		Issuer:      issuer,
		Sessions:    sessions,
		Uploads:     uploads,
		Objects:     objects,
		Connections: connections,
		Consent:     consentController{consent},
		Diagnostics: newDiagnostics(app, adminScope, backends, faults.Enabled()),
		Folders:     connections,
		Objects_:    persistence.NewObjectRepository(store),
		Routing:     routingSwitch,
		Faults:      faults,
		Logger:      logger,
	})

	// The worker is part of the process, not a separate binary, so a deployment
	// cannot run without it by accident. It stops with the app.
	// Background work gets its own cancellable context so Stop is self-sufficient.
	bgCtx, bgCancel := context.WithCancel(ctx)
	app.bgCancel = bgCancel

	poller := NewChangesPoller(driveClient, logger)
	poller.Interval = cfg.Worker.Poll.Duration() * 4
	app.PollerStop = StartChangesPoller(bgCtx, poller, cfg.Worker.Enabled, driveClient.ConnectedIDs)

	app.WorkerStop = StartWorker(bgCtx, NewWorker(store, uploads, objects, backends, appMetrics, logger, WorkerConfig{
		Enabled:     cfg.Worker.Enabled,
		Owner:       cfg.Worker.Owner,
		Lease:       cfg.Worker.Lease.Duration(),
		Poll:        cfg.Worker.Poll.Duration(),
		MaxAttempts: cfg.Worker.MaxAttempts,
	}), cfg.Worker.Enabled)

	app.Connections = connections
	app.Drive = driveClient
	app.Issuer = issuer
	app.Sessions = sessions

	app.Server = buildServer(cfg, logger, store, issuer, sessions, uploads, objects, connections, consent, console, driveClient, cacheBackend, appMetrics)

	app.DriveFaultsEnabled = driveClient.FaultsEnabled()
	if cfg.AdminPanel.SimulateDriveOutage && cfg.Environment == "development" {
		logger.WarnContext(ctx, "drive outage simulation is ACTIVE; new uploads will route to the fallback provider")
	}

	logger.InfoContext(ctx, "storvia service configured",
		"summary", cfg.RedactedSummary(),
		"key_references", keyringReferences(keyring),
		"issues", app.Issues,
	)
	return app, nil
}

// credentialNames returns the environment variable names holding one vendor's
// fallback credentials.
//
// The names come from the vendor, not from configuration and not from the backend
// identifier, so an R2 backend cannot be pointed at the AWS credentials by mistake.
func credentialNames(vendor string) struct{ id, secret, token string } {
	if vendor == config.VendorAWSS3 {
		return struct{ id, secret, token string }{
			config.EnvAWSAccessKeyID, config.EnvAWSSecretAccessKey, ""}
	}
	return struct{ id, secret, token string }{
		config.EnvR2AccessKeyID, config.EnvR2SecretAccessKey, config.EnvR2SessionToken}
}

// buildCache constructs the configured cache backend.
func buildCache(ctx context.Context, cfg config.CacheConfig, resolved map[string]string) (cache.Cache, error) {
	switch cfg.Driver {
	case config.DriverMemory:
		return cache.NewMemory(cache.WithJanitorInterval(cfg.JanitorInterval.Duration())), nil
	case config.DriverRedis:
		return cache.NewRedis(ctx, cfg.RedisAddress,
			cache.WithPassword(resolved[config.EnvRedisPassword]),
			cache.WithDatabase(cfg.RedisDatabase),
			cache.WithClientName("storvia-api"),
		)
	default:
		return nil, fmt.Errorf("app: unsupported cache driver %q", cfg.Driver)
	}
}

// buildBackends constructs the S3-compatible clients.
//
// Credentials come from the environment via the config's env references, which
// were already resolved at load time. A backend that fails to construct is
// reported as an issue and skipped: it must not take the whole service down, and
// it must not silently become a no-op that the routing policy thinks is healthy.
func buildBackends(ctx context.Context, cfg *config.Config, resolved map[string]string, logger *slog.Logger) (map[string]*s3compat.Client, []string) {
	var issues []string
	built := make(map[string]*s3compat.Client)

	if !cfg.Fallback.Enabled {
		return built, issues
	}

	for _, backendCfg := range cfg.Fallback.ActiveBackends() {
		client, err := s3compat.New(ctx, s3compat.Config{
			ID:            backendCfg.ID,
			Vendor:        backendCfg.Vendor,
			Endpoint:      backendCfg.Endpoint,
			Region:        backendCfg.Region,
			Bucket:        backendCfg.Bucket,
			Prefix:        backendCfg.Prefix,
			UsePathStyle:  backendCfg.UsePathStyle,
			PresignTTL:    cfg.Uploads.PresignedURLTTL.Duration(),
			RetryAttempts: 2,
			Credentials: s3compat.Credentials{
				AccessKeyID:     resolved[credentialNames(backendCfg.Vendor).id],
				SecretAccessKey: resolved[credentialNames(backendCfg.Vendor).secret],
				SessionToken:    resolved[credentialNames(backendCfg.Vendor).token],
			},
		}, s3compat.Options{MultipartTTL: backendCfg.MultipartAbortTTL.Duration()})
		if err != nil {
			issues = append(issues, fmt.Sprintf("fallback backend %q could not be constructed: %v", backendCfg.ID, err))
			logger.ErrorContext(ctx, "fallback backend construction failed",
				"backend_id", backendCfg.ID, "error", err.Error())
			continue
		}
		built[backendCfg.ID] = client
	}
	return built, issues
}

// buildKeyring constructs the encryption keyring.
func buildKeyring(cfg *config.Config, resolved map[string]string) (*secrets.AESGCMKeyring, error) {
	// A missing key was already reported by ResolveSecrets; this is a backstop.
	active := resolved[config.EnvEncryptionKey]
	if active == "" {
		return nil, fmt.Errorf("app: the encryption key %q is not set; refresh tokens cannot be stored safely",
			config.EnvEncryptionKey)
	}
	keyring, err := secrets.NewAESGCMKeyring(secrets.KeyringOptions{
		ActiveReference:   config.EnvEncryptionKey,
		ActiveKey:         active,
		PreviousReference: config.EnvEncryptionKeyPrevious,
		PreviousKey:       resolved[config.EnvEncryptionKeyPrevious],
	})
	if err != nil {
		return nil, fmt.Errorf("app: encryption keyring: %w", err)
	}
	return keyring, nil
}

// buildDrive constructs the Drive provider.
func buildDrive(ctx context.Context, cfg *config.Config, store *persistence.Store, resolved map[string]string, logger *slog.Logger, keyring secrets.Keyring) (*drive.Client, error) {
	connections := persistence.NewDriveConnectionRepository(store, keyring)

	client, err := drive.New(ctx, drive.Config{
		Credentials: drive.Credentials{
			ClientID:     resolved[config.EnvGoogleClientID],
			ClientSecret: resolved[config.EnvGoogleClientSecret],
			RedirectURI:  cfg.Drive.RedirectURI,
		},
		Scopes:           cfg.Drive.Scopes,
		RootFolderName:   cfg.Drive.RootFolderName,
		PreflightTimeout: cfg.Drive.PreflightTimeout.Duration(),
	}, drive.ClientDeps{
		Tokens:   drive.NewMemoryTokens(),
		Resolver: drive.NewResolver(connections),
		States:   drive.NewStateStore(10 * time.Minute),
		Logger:   logger,
		// Fault injection is gated on the environment, never on configuration. A
		// config file can be copied from development into production by accident;
		// an environment cannot.
		Faults: drive.NewFaultInjection(
			cfg.Environment == "development", cfg.AdminPanel.SimulateDriveOutage),
	})
	if err != nil {
		return nil, fmt.Errorf("app: drive client: %w", err)
	}
	return client, nil
}

// buildServices constructs the use cases.
func buildServices(cfg *config.Config, store *persistence.Store, keyring secrets.Keyring, driveClient *drive.Client, backends map[string]*s3compat.Client, logger *slog.Logger, appMetrics *metrics.Storvia, routingSwitch *RoutingSwitch) (*service.UploadService, *service.ObjectService, *service.ConnectionService, *service.ConsentService, error) {
	objects := persistence.NewObjectRepository(store)
	uploadsRepo := persistence.NewUploadRepository(store)

	// The outage-fallback gate reads a tenant's recorded acceptance.
	//
	// This was previously never wired, so FallbackConsent fell through to its
	// default of "false" and the fallback could not fire in a real deployment at
	// all, however the fallback was configured. A deployment that enables the
	// fallback has to obtain consent to benefit from it.
	consentRepo := persistence.NewFallbackConsentRepository(store)
	attempts := persistence.NewAttemptRepository(store)
	backendRepo := persistence.NewBackendRepository(store)

	// Record the enabled backends so the routing policy can confirm an eligible
	// fallback has a real destination before it selects one.
	for _, backendCfg := range cfg.Fallback.ActiveBackends() {
		if _, ok := backends[backendCfg.ID]; !ok {
			continue
		}
		scopeValue := "deployment"
		err := backendRepo.Upsert(context.Background(), persistence.Backend{
			ID:           backendCfg.ID,
			Scope:        scopeValue,
			Vendor:       backendCfg.Vendor,
			Endpoint:     backendCfg.Endpoint,
			Region:       backendCfg.Region,
			Bucket:       backendCfg.Bucket,
			Prefix:       backendCfg.Prefix,
			Status:       "active",
			Capabilities: "{}",
		})
		if err != nil {
			logger.Warn("could not record the fallback backend", "backend_id", backendCfg.ID, "error", err.Error())
		}
	}

	uploads, err := service.NewUploadService(service.Dependencies{
		Objects:          objects,
		Uploads:          uploadsRepo,
		Attempts:         attempts,
		Backends:         backendRepo,
		DriveSessions:    newDriveSessions(driveClient),
		FallbackUploader: newFallbackUploader(backends, int64(cfg.Uploads.S3PartSizeBytes), cfg.Uploads.PresignedURLTTL.Duration()),
		Drive:            driveClient,
		UploadLimits:     cfg.Uploads,
		DriveCfg:         cfg.Drive,
		Fallback:         cfg.Fallback,
		IDs:              idid.NewGenerator("upl"),
		Logger:           logger,
		FallbackConsent:  service.ConsentGate(consentRepo, cfg.Fallback.ConsentRequired),
		// Metrics was left unwired for a while, so every upload counter was recording
		// into a nil pointer and the families stayed permanently empty. The nil-safety
		// is still correct -- a deployment without metrics should not fail requests --
		// but nil must mean "not built", never "built and forgotten".
		Metrics:       appMetrics,
		ForceFallback: routingSwitch.Forced,
	})
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("app: upload service: %w", err)
	}

	source := newContentSource(store, backends, driveClient, logger)
	objectService := service.NewObjectService(objects, uploadsRepo, source).
		WithJobs(persistence.NewJobRepository(store), logger)
	connectionService := service.NewConnectionService(service.NewDriveConnector(driveClient))

	appConsent := service.NewConsentService(consentRepo)

	return uploads, objectService, connectionService, appConsent, nil
}

// buildAuth constructs the session issuer and store.
func buildAuth(ctx context.Context, cfg *config.Config, resolved map[string]string, cacheBackend cache.Cache) (*auth.Issuer, auth.Lookup, error) {
	issuer, err := auth.NewIssuer(auth.IssuerOptions{
		SigningKey: resolved[config.EnvSessionSigningKey],
		CookieName: cfg.Auth.SessionCookieName,
		TTL:        cfg.Auth.SessionTTL.Duration(),
		Secure:     cfg.Auth.SecureCookie,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("app: session issuer: %w", err)
	}

	sessions, err := buildSessionStore(ctx, cfg, resolved, cacheBackend)
	if err != nil {
		return nil, nil, err
	}
	return issuer, sessions, nil
}

// buildSessionStore chooses where sessions live.
//
// The in-memory store is correct for a single process and silently wrong for two: a
// token issued by one instance is invisible to the other, so every second request
// fails. That failure looks like an intermittent authentication bug, which is a
// miserable thing to debug, so a Redis-backed store is used whenever the deployment
// already has Redis.
//
// The TTL is the session lifetime plus a margin, because a key that expires before its
// token does logs the user out while their cookie is still valid.
func buildSessionStore(ctx context.Context, cfg *config.Config, resolved map[string]string, cacheBackend cache.Cache) (auth.Lookup, error) {
	if cacheBackend == nil {
		return auth.NewMemorySessions(10000), nil
	}
	redisCache, ok := cacheBackend.(*cache.Redis)
	if !ok {
		// Memory cache, so this is a single-process deployment and memory sessions
		// are correct.
		return auth.NewMemorySessions(10000), nil
	}

	client, err := redisCache.Client(ctx)
	if err != nil {
		// Falling back to memory here would be the dangerous choice: it would start
		// working and quietly log users out at random as instances disagreed. Better
		// to refuse to start.
		return nil, fmt.Errorf("app: the session store needs the redis client: %w", err)
	}
	_ = resolved

	return auth.NewRedisSessions(client,
		auth.WithSessionPrefix("storvia:session:"),
		auth.WithSessionTTL(cfg.Auth.SessionTTL.Duration()+time.Hour),
	), nil
}

// buildServer assembles the HTTP server.
func buildServer(cfg *config.Config, logger *slog.Logger, store *persistence.Store, issuer *auth.Issuer, sessions auth.Lookup, uploads *service.UploadService, objects *service.ObjectService, connections *service.ConnectionService, consent *service.ConsentService, console *httpapi.Console, driveClient *drive.Client, cacheBackend cache.Cache, appMetrics *metrics.Storvia) *http.Server {
	ready := func(ctx context.Context) error {
		// The session backend is part of readiness. Without this, a Redis outage
		// leaves /readyz green while every authenticated request fails, and traffic
		// keeps arriving.
		if redisSessions, ok := sessions.(*auth.RedisSessions); ok {
			if err := redisSessions.Ping(ctx); err != nil {
				return err
			}
		}

		// Readiness consults the database. Liveness deliberately does not: a
		// database blip should not get the process restarted into the same blip,
		// which would turn a dependency problem into an outage loop.
		return store.Ping(ctx)
	}

	handler := httpapi.Router(httpapi.Config{
		Issuer:         issuer,
		Sessions:       sessions,
		Logger:         logger,
		Uploads:        httpapi.NewUploadHandler(uploads, driveClient, cfg.Uploads),
		Consent:        httpapi.NewConsentHandler(consent),
		Objects:        httpapi.NewObjectHandler(objects),
		Connections:    httpapi.NewConnectionHandler(connections),
		Console:        console,
		Health:         httpapi.NewHealthHandler(ready, cacheBackend).WithMetrics(appMetrics),
		AllowedOrigins: cfg.Security.AllowedOrigins,
		UploadTimeout:  cfg.Server.UploadTimeout.Duration(),
		// Metadata requests get the routing budget, which is generous enough for a
		// preflight plus a database round trip.
		MetadataTimeout: cfg.Drive.RoutingBudget.Duration(),
		RateLimit:       600,
		RateInterval:    time.Minute,
	})

	return &http.Server{
		Addr:              cfg.Server.Addr(),
		Handler:           handler,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Duration(),
		ReadTimeout:       cfg.Server.ReadTimeout.Duration(),
		WriteTimeout:      cfg.Server.WriteTimeout.Duration(),
		IdleTimeout:       cfg.Server.IdleTimeout.Duration(),
		MaxHeaderBytes:    1 << 20,
	}
}

// keyringReferences lists configured key references for the startup log.
func keyringReferences(keyring *secrets.AESGCMKeyring) []string { return keyring.References() }

// releaseVersion reports the build identifier recorded in configuration.
//
// It is "unknown" rather than empty when unset, because an empty label value in a
// scrape is easy to misread as a different build.
func releaseVersion(cfg *config.Config) string {
	if cfg == nil || cfg.Service.Release == "" {
		return "unknown"
	}
	return cfg.Service.Release
}

// StopBackground ends the worker and the change poller and waits for both.
//
// It cancels first and then waits. Cancelling first is what makes waiting terminate;
// the reverse order deadlocks whenever the caller's context was never cancelled.
func (a *App) StopBackground() {
	if a == nil {
		return
	}
	if a.bgCancel != nil {
		a.bgCancel()
	}
	if a.PollerStop != nil {
		a.PollerStop()
	}
	if a.WorkerStop != nil {
		a.WorkerStop()
	}
}
