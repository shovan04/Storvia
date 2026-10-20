// Package config loads Storvia's JSON configuration and resolves every secret
// reference from the process environment.
//
// The split is deliberate and is the project's rule: JSON files hold structure
// and non-secret values and are safe to review and commit, while .env files hold
// API keys, database passwords, and signing material and never enter version
// control. A JSON string of the form ${VAR} or ${VAR:-fallback} is resolved at
// load time, which keeps a config file readable without ever hard-coding a
// credential in it.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Config is the whole service configuration.
type Config struct {
	Environment string           `json:"environment"`
	Service     ServiceConfig    `json:"service"`
	Server      ServerConfig     `json:"server"`
	Logging     LoggingConfig    `json:"logging"`
	Database    DatabaseConfig   `json:"database"`
	Cache       CacheConfig      `json:"cache"`
	Auth        AuthConfig       `json:"auth"`
	AdminPanel  AdminPanelConfig `json:"admin_panel"`
	Security    SecurityConfig   `json:"security"`
	Uploads     UploadsConfig    `json:"uploads"`
	Drive       DriveConfig      `json:"drive"`
	Fallback    FallbackConfig   `json:"fallback"`
	Worker      WorkerConfig     `json:"worker"`
}

// ServiceConfig carries build identity stamped onto every log record.
type ServiceConfig struct {
	Name    string `json:"name"`
	Release string `json:"release"`
}

// ServerConfig controls the HTTP listener and its timeouts.
//
// The upload deadline is intentionally separate from the routing budget: a slow
// client pushing a large file must never be classified as a Drive outage.
type ServerConfig struct {
	Host              string   `json:"host"`
	Port              int      `json:"port"`
	ReadHeaderTimeout Duration `json:"read_header_timeout"`
	ReadTimeout       Duration `json:"read_timeout"`
	WriteTimeout      Duration `json:"write_timeout"`
	IdleTimeout       Duration `json:"idle_timeout"`
	UploadTimeout     Duration `json:"upload_timeout"`
	ShutdownGrace     Duration `json:"shutdown_grace"`
}

// Addr returns the listen address.
func (s ServerConfig) Addr() string { return fmt.Sprintf("%s:%d", s.Host, s.Port) }

// LoggingConfig controls slog output.
type LoggingConfig struct {
	Level  string `json:"level"`
	Format string `json:"format"`
}

// DatabaseConfig selects the relational metadata store.
//
// SQLite is for development and single-node evaluation; PostgreSQL is the
// production target.
//
// The connection parameters are split by secrecy, not by convenience. Host, port,
// user, and database name are not secrets and live here as plain values, because
// putting them behind an environment reference would push ordinary configuration
// into a secrets file for no benefit. Only the password is a secret, and only the
// password is referenced.
type DatabaseConfig struct {
	Driver string `json:"driver"`

	// SQLitePath is the SQLite file location. Not a secret.
	SQLitePath string `json:"sqlite_path"`

	// Postgres connection parameters. Not secrets.
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Database string `json:"database"`
	// SSLMode is passed through to the driver. Not a secret.
	SSLMode string `json:"sslmode"`

	MaxOpenConns    int      `json:"max_open_conns"`
	MaxIdleConns    int      `json:"max_idle_conns"`
	ConnMaxLifetime Duration `json:"conn_max_lifetime"`
	ConnMaxIdleTime Duration `json:"conn_max_idle_time"`
	AutoMigrate     bool     `json:"auto_migrate"`
}

// BuildDSN assembles the driver connection string from the plain parameters and
// the resolved password.
//
// It is built here rather than read whole from configuration because a complete
// DSN contains a credential, and a complete DSN in a checked-in file is a
// credential in version control.
func (c DatabaseConfig) BuildDSN(password string) string {
	switch c.Driver {
	case DriverSQLite:
		path := c.SQLitePath
		if path == "" {
			path = "file::memory:"
		}
		return path
	default:
		sslMode := c.SSLMode
		if sslMode == "" {
			sslMode = "require"
		}
		return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s",
			c.User, url.QueryEscape(password), c.Host, c.Port, c.Database, sslMode)
	}
}

// DriverSQLite and DriverPostgres are the accepted database drivers.
const (
	DriverSQLite   = "sqlite"
	DriverPostgres = "postgres"
)

// CacheConfig selects the cache backend. Memory is the default; Redis is for
// multi-instance deployments that must share token and circuit state.
//
// The address is not a secret and is a plain value. Only the password is a secret,
// so only the password is a reference.
type CacheConfig struct {
	Driver       string `json:"driver"`
	RedisAddress string `json:"redis_address"`
	// RedisPasswordRef names the environment variable holding the Redis password.
	RedisDatabase int `json:"redis_database"`

	JanitorInterval   Duration `json:"janitor_interval"`
	AvailabilityScope string   `json:"availability_scope"`
}

// DriverMemory and DriverRedis are the accepted cache drivers.
const (
	DriverMemory = "memory"
	DriverRedis  = "redis"
)

// AuthConfig controls application sign-in. Application identity is deliberately
// separate from Google Drive authorization.
type AuthConfig struct {
	SessionCookieName string   `json:"session_cookie_name"`
	SessionTTL        Duration `json:"session_ttl"`
	// AdminSessionTTL is the lifetime of the admin panel's sign-in cookie.
	//
	// It is separate from SessionTTL and much longer on purpose. An operator closing
	// the panel for a day should not find themselves logged out and having to run a
	// Google consent round trip again to look at a queue. The session is still
	// revocable at any moment through the panel's sign-out button, so a long lifetime
	// does not mean an inescapable session.
	AdminSessionTTL Duration `json:"admin_session_ttl"`
	SecureCookie    bool     `json:"secure_cookie"`
}

// AdminPanelConfig controls the local development panel used to exercise the
// Google Drive consent flow. It is intentionally separate from application
// authentication: the panel mints a normal Storvia session only after Google
// completes the Drive OAuth exchange.
type AdminPanelConfig struct {
	Enabled    bool   `json:"enabled"`
	TenantID   string `json:"tenant_id"`
	UserID     string `json:"user_id"`
	FlowCookie string `json:"flow_cookie"`

	// SimulateDriveOutage forces the read-only Drive availability preflight to
	// report a transport failure, so the outage-routing path can be exercised on
	// demand instead of waiting for a real Google incident.
	//
	// It is refused outside development. A provider that can be told to lie about
	// its own health is a way to disable the service's safety checks, so this is a
	// stop-the-line error rather than a warning.
	SimulateDriveOutage bool `json:"simulate_drive_outage,omitempty"`

	// ConsoleURL is where the browser is sent after Go has verified the OAuth callback
	// and issued the session.
	//
	// It is empty when this binary serves the console itself. It is absolute when the
	// console is a separate frontend process, because that frontend is not proxied
	// through this server and Go is the only thing that can return the browser to it.
	//
	// STORVIA_CONSOLE_URL overrides it, so the development launcher can point at
	// whichever port the frontend happens to be on without editing this file.
	ConsoleURL string `json:"console_url,omitempty"`
}

// EnvConsoleURL overrides admin_panel.console_url.
const EnvConsoleURL = "STORVIA_CONSOLE_URL"

// SecurityConfig holds references to key material. Values arrive from the
// environment and are never written back to a config file or a log.
type SecurityConfig struct {
	AllowedOrigins         []string `json:"allowed_origins"`
	AllowedEndpointSchemes []string `json:"allowed_endpoint_schemes"`
	AllowedEndpointHosts   []string `json:"allowed_endpoint_hosts"`
}

// UploadsConfig bounds what a tenant may store and how transfer proceeds.
type UploadsConfig struct {
	MaxFileSizeBytes    int64            `json:"max_file_size_bytes"`
	AllowedMIMETypes    []string         `json:"allowed_mime_types"`
	AllowedCategories   []string         `json:"allowed_categories"`
	DriveChunkSizeBytes int              `json:"drive_chunk_size_bytes"`
	S3PartSizeBytes     int              `json:"s3_part_size_bytes"`
	PresignedURLTTL     Duration         `json:"presigned_url_ttl"`
	MinPartSizeBytes    int              `json:"min_part_size_bytes"`
	CategoryLimits      map[string]int64 `json:"category_limits"`
}

// CategoryAllowed reports whether category is accepted.
func (u UploadsConfig) CategoryAllowed(category string) bool {
	for _, allowed := range u.AllowedCategories {
		if allowed == category {
			return true
		}
	}
	return false
}

// MIMEAllowed reports whether mimeType passes the allow-list. Client-declared
// MIME is untrusted metadata used for routing and download headers only; it is
// never treated as content validation.
func (u UploadsConfig) MIMEAllowed(mimeType string) bool {
	if len(u.AllowedMIMETypes) == 0 {
		return false
	}
	for _, allowed := range u.AllowedMIMETypes {
		if strings.EqualFold(allowed, mimeType) {
			return true
		}
	}
	return false
}

// EffectiveMaxSize returns the category-specific limit, falling back to the
// global maximum when the category has none.
func (u UploadsConfig) EffectiveMaxSize(category string) int64 {
	if limit, ok := u.CategoryLimits[category]; ok && limit > 0 && limit < u.MaxFileSizeBytes {
		return limit
	}
	return u.MaxFileSizeBytes
}

// DriveConfig holds Google OAuth settings and the availability routing budget.
//
// Every secret is an environment reference. The narrow drive.file scope is the
// only scope the MVP requests. When StoreInAppDataFolder is true, files are
// stored in Drive's appDataFolder and are invisible to the user in Drive web
// and mobile; this requires the drive.appdata scope.
type DriveConfig struct {
	RedirectURI          string   `json:"redirect_uri"`
	AdminRedirectURI     string   `json:"admin_redirect_uri"`
	Scopes               []string `json:"scopes"`
	RootFolderName       string   `json:"root_folder_name"`
	StoreInAppDataFolder bool     `json:"store_in_app_data_folder"`
	PreflightTimeout     Duration `json:"preflight_timeout"`
	PreflightAttempts    int      `json:"preflight_attempts"`
	RoutingBudget        Duration `json:"routing_budget"`
	CircuitFailures      int      `json:"circuit_failures"`
	CircuitCooldown      Duration `json:"circuit_cooldown"`
}

// WorkerConfig tunes the background job runner.
//
// It is ordinary configuration, so it belongs in the JSON. There is deliberately no
// secret here: the worker's identity is a lease owner string, not a credential.
type WorkerConfig struct {
	// Enabled turns the worker on. It is on by default because a queue nothing
	// drains is worse than no queue: a failed delete would sit in `deleting`
	// forever and every retry would read as a conflict.
	Enabled bool `json:"enabled"`
	// Owner identifies this worker in the lease. Empty means hostname and pid.
	Owner string `json:"owner"`
	// Lease is how long a claimed job is reserved.
	Lease Duration `json:"lease"`
	// Poll is how long to wait when there is nothing to do.
	Poll Duration `json:"poll"`
	// MaxAttempts before a failing job is parked for a person.
	MaxAttempts int `json:"max_attempts"`
}

// FallbackConfig lists the S3-compatible backends eligible for outage uploads.
// One backend is active per deployment at first; selection is operator
// configuration and never client supplied.
type FallbackConfig struct {
	Enabled         bool              `json:"enabled"`
	ConsentRequired bool              `json:"consent_required"`
	Backends        []S3BackendConfig `json:"backends"`
	// Force routes every new upload to the fallback provider even when Drive is
	// healthy.
	//
	// This is an operator decision, not a fault injection. Fault injection pretends
	// Drive is broken so the fallback path can be exercised; this actually prefers
	// the fallback, which is what you want when Drive is refusing traffic and the
	// question is whether users can keep working at all.
	//
	// It never overrides consent, authorization, request validity, or the rule that an
	// upload whose bytes may already have reached a provider stays pinned there.
	Force bool `json:"force"`
}

// S3BackendConfig is one S3-compatible destination.
//
// Vendor is informational and selects capability behaviour; the adapter targets
// the common S3 subset only. Endpoint and credentials are operator configuration,
// which is what prevents a client from steering writes at an arbitrary host.
type S3BackendConfig struct {
	ID     string `json:"id"`
	Vendor string `json:"vendor"`
	// Endpoint overrides the vendor's environment endpoint for this backend. It is
	// optional: leaving it empty reads EndpointEnvVar(Vendor) from the environment.
	// It exists for deployments running two backends of the same vendor against
	// different endpoints, where a single vendor-level variable cannot say which is
	// which.
	Endpoint          string   `json:"endpoint,omitempty"`
	Region            string   `json:"region"`
	Bucket            string   `json:"bucket"`
	Prefix            string   `json:"prefix"`
	UsePathStyle      bool     `json:"use_path_style"`
	Disabled          bool     `json:"disabled"`
	MultipartAbortTTL Duration `json:"multipart_abort_ttl"`
}

// VendorCloudflareR2 and VendorAWSS3 are the vendors Storvia recognizes.
const (
	VendorCloudflareR2 = "cloudflare_r2"
	VendorAWSS3        = "aws_s3"
)

// ActiveBackends returns the backends that are neither disabled nor missing a
// required field.
func (f FallbackConfig) ActiveBackends() []S3BackendConfig {
	out := make([]S3BackendConfig, 0, len(f.Backends))
	for _, b := range f.Backends {
		if b.Disabled || b.Bucket == "" {
			continue
		}
		out = append(out, b)
	}
	return out
}

// envPattern matches ${NAME} and ${NAME:-default}.
var envPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// ExpandEnv replaces ${NAME} and ${NAME:-default} in raw with values from lookup.
//
// A reference with no value and no default is an error, not an empty string: a
// silently empty access key is how a fallback backend ends up misconfigured in
// production.
func ExpandEnv(raw []byte, lookup func(string) (string, bool)) ([]byte, error) {
	var missing []string
	out := envPattern.ReplaceAllFunc(raw, func(match []byte) []byte {
		groups := envPattern.FindSubmatch(match)
		name := string(groups[1])
		if value, ok := lookup(name); ok && value != "" {
			return []byte(value)
		}
		if groups[2] != nil {
			return groups[2]
		}
		missing = append(missing, name)
		return nil
	})
	if len(missing) > 0 {
		return nil, fmt.Errorf("config: unresolved environment reference(s): %s",
			strings.Join(dedupe(missing), ", "))
	}
	return out, nil
}

func dedupe(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// Load reads and validates a JSON configuration file.
//
// Unknown fields are rejected. A typo in a security-relevant key must fail at
// startup rather than silently falling back to a zero value that disables a
// control.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	return Parse(raw, os.LookupEnv)
}

// Parse decodes configuration from raw bytes, resolving environment references
// through lookup. Tests use it directly with a map-backed lookup.
func Parse(raw []byte, lookup func(string) (string, bool)) (*Config, error) {
	expanded, err := ExpandEnv(raw, lookup)
	if err != nil {
		return nil, err
	}

	var cfg Config
	decoder := json.NewDecoder(bytes.NewReader(expanded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: decode: %w", err)
	}
	cfg.applyDefaults()

	// The endpoint lives in the environment, so it is resolved before validation: the
	// allow-list check has nothing to inspect until it has a value.
	if err := cfg.resolveEndpoints(lookup); err != nil {
		return nil, err
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// resolveEndpoints fills each backend's endpoint from the environment.
//
// A value already present in the JSON wins, so a deployment with two backends of one
// vendor can still name them individually. A missing value is an error rather than an
// empty string: an empty endpoint would silently send requests to the AWS default,
// which for an R2 backend means an unconfigured client rather than a clear error.
func (c *Config) resolveEndpoints(lookup func(string) (string, bool)) error {
	for i := range c.Fallback.Backends {
		backend := &c.Fallback.Backends[i]
		if backend.Disabled {
			continue
		}
		if backend.Endpoint != "" {
			continue
		}
		name := EndpointEnvVar(backend.Vendor)
		value, ok := lookup(name)
		if !ok || value == "" {
			return fmt.Errorf("config: fallback backend %q (%s) has no endpoint: "+
				"set %s in the environment, or give the backend an explicit endpoint",
				backend.ID, backend.Vendor, name)
		}
		backend.Endpoint = value
	}
	return nil
}

// applyDefaults fills optional values. It never invents a secret: a missing
// credential must stay missing so Validate can report it.
func (c *Config) applyDefaults() {
	if c.Service.Name == "" {
		c.Service.Name = "storvia-api"
	}
	if c.Worker.Lease.Duration() == 0 {
		c.Worker.Lease = Duration(2 * time.Minute)
	}
	if c.Worker.Poll.Duration() == 0 {
		c.Worker.Poll = Duration(5 * time.Second)
	}
	if c.Worker.MaxAttempts <= 0 {
		c.Worker.MaxAttempts = 5
	}
	if c.Server.Host == "" {
		c.Server.Host = "127.0.0.1"
	}
	if c.Server.Port == 0 {
		c.Server.Port = 8080
	}
	if c.Server.ReadHeaderTimeout == 0 {
		c.Server.ReadHeaderTimeout = Duration(10 * time.Second)
	}
	if c.Server.ReadTimeout == 0 {
		c.Server.ReadTimeout = Duration(60 * time.Second)
	}
	if c.Server.WriteTimeout == 0 {
		c.Server.WriteTimeout = 0 // streaming downloads manage their own deadline
	}
	if c.Server.IdleTimeout == 0 {
		c.Server.IdleTimeout = Duration(120 * time.Second)
	}
	if c.Server.UploadTimeout == 0 {
		c.Server.UploadTimeout = Duration(30 * time.Minute)
	}
	if c.Server.ShutdownGrace == 0 {
		c.Server.ShutdownGrace = Duration(30 * time.Second)
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.Format == "" {
		c.Logging.Format = "json"
	}
	if c.Database.MaxOpenConns == 0 {
		c.Database.MaxOpenConns = 10
	}
	if c.Database.MaxIdleConns == 0 {
		c.Database.MaxIdleConns = 5
	}
	if c.Database.ConnMaxLifetime == 0 {
		c.Database.ConnMaxLifetime = Duration(30 * time.Minute)
	}
	if c.Database.ConnMaxIdleTime == 0 {
		c.Database.ConnMaxIdleTime = Duration(5 * time.Minute)
	}
	if c.Cache.Driver == "" {
		c.Cache.Driver = DriverMemory
	}
	if c.Cache.JanitorInterval == 0 {
		c.Cache.JanitorInterval = Duration(time.Minute)
	}
	if c.Auth.SessionCookieName == "" {
		c.Auth.SessionCookieName = "storvia_session"
	}
	if c.Auth.SessionTTL == 0 {
		c.Auth.SessionTTL = Duration(12 * time.Hour)
	}
	if c.Auth.AdminSessionTTL == 0 {
		c.Auth.AdminSessionTTL = Duration(30 * 24 * time.Hour)
	}
	// A panel session that outlives the API session is fine; the reverse is not,
	// because the cookie would outlive the server-side record and every request
	// would fail as an expired session rather than as a clear "signed out".
	if c.Auth.SessionTTL > 0 && c.Auth.AdminSessionTTL < c.Auth.SessionTTL {
		c.Auth.AdminSessionTTL = c.Auth.SessionTTL
	}
	if c.AdminPanel.FlowCookie == "" {
		c.AdminPanel.FlowCookie = "storvia_admin_flow"
	}
	if len(c.Security.AllowedEndpointSchemes) == 0 {
		c.Security.AllowedEndpointSchemes = []string{"https"}
	}
	if c.Uploads.MaxFileSizeBytes == 0 {
		c.Uploads.MaxFileSizeBytes = 5 << 30 // 5 GiB
	}
	if c.Uploads.DriveChunkSizeBytes == 0 {
		// 8 MiB: a multiple of Drive's 256 KiB chunk requirement with enough
		// headroom to keep request count low.
		c.Uploads.DriveChunkSizeBytes = 8 << 20
	}
	if c.Uploads.S3PartSizeBytes == 0 {
		c.Uploads.S3PartSizeBytes = 8 << 20
	}
	if c.Uploads.MinPartSizeBytes == 0 {
		c.Uploads.MinPartSizeBytes = 5 << 20
	}
	if c.Uploads.PresignedURLTTL == 0 {
		c.Uploads.PresignedURLTTL = Duration(15 * time.Minute)
	}
	if c.Drive.RootFolderName == "" {
		c.Drive.RootFolderName = "Storvia"
	}
	if len(c.Drive.Scopes) == 0 {
		c.Drive.Scopes = []string{"https://www.googleapis.com/auth/drive.file"}
	}
	if c.Drive.PreflightTimeout == 0 {
		c.Drive.PreflightTimeout = Duration(3 * time.Second)
	}
	if c.Drive.PreflightAttempts == 0 {
		c.Drive.PreflightAttempts = 2
	}
	if c.Drive.RoutingBudget == 0 {
		c.Drive.RoutingBudget = Duration(8 * time.Second)
	}
	if c.Drive.CircuitFailures == 0 {
		c.Drive.CircuitFailures = 2
	}
	if c.Drive.CircuitCooldown == 0 {
		c.Drive.CircuitCooldown = Duration(30 * time.Second)
	}
}

// Validate reports configuration that would be unsafe or non-functional.
func (c *Config) Validate() error {
	if err := c.validateDatabase(); err != nil {
		return err
	}
	if err := c.validateCache(); err != nil {
		return err
	}
	if err := c.validateDrive(); err != nil {
		return err
	}
	if err := c.validateAdminPanel(); err != nil {
		return err
	}
	if err := c.validateWorker(); err != nil {
		return err
	}
	if err := c.validateFallback(); err != nil {
		return err
	}
	if err := c.validateUploads(); err != nil {
		return err
	}
	return nil
}

func (c *Config) validateAdminPanel() error {
	// Fault injection is checked before the enabled flag, so it is refused even
	// when the panel itself is off. Otherwise a configuration could enable a fault
	// and leave the panel disabled, which looks harmless and is not.
	if c.AdminPanel.SimulateDriveOutage && c.Environment != "development" {
		return fmt.Errorf("config: admin_panel.simulate_drive_outage is permitted only in the " +
			"development environment. Fault injection that can ship is a way to disable the " +
			"service's own safety checks")
	}
	if !c.AdminPanel.Enabled {
		return nil
	}
	if c.Environment != "development" {
		return fmt.Errorf("config: admin_panel.enabled is permitted only in the development environment")
	}
	if strings.TrimSpace(c.AdminPanel.TenantID) == "" || strings.TrimSpace(c.AdminPanel.UserID) == "" {
		return fmt.Errorf("config: admin_panel.tenant_id and admin_panel.user_id are required when the panel is enabled")
	}
	if strings.TrimSpace(c.Drive.AdminRedirectURI) == "" {
		return fmt.Errorf("config: drive.admin_redirect_uri is required when the admin panel is enabled")
	}
	if strings.TrimSpace(c.AdminPanel.FlowCookie) == "" {
		return fmt.Errorf("config: admin_panel.flow_cookie must not be empty")
	}
	return nil
}

func (c *Config) validateDatabase() error {
	switch c.Database.Driver {
	case DriverSQLite:
		// SQLite is a development driver. One writer at a time avoids
		// SQLITE_BUSY under concurrent uploads; the job-lease queries use the
		// dialect's own conditional update instead of SKIP LOCKED.
		if c.Database.MaxOpenConns > 1 {
			c.Database.MaxOpenConns = 1
		}
		if c.Database.SQLitePath == "" {
			return fmt.Errorf("config: database.sqlite_path is required for the %s driver", DriverSQLite)
		}
	case DriverPostgres:
		// Every connection parameter except the password is ordinary
		// configuration and must be present as a plain value.
		if c.Database.Host == "" {
			return fmt.Errorf("config: database.host is required for the %s driver", DriverPostgres)
		}
		if c.Database.Port == 0 {
			return fmt.Errorf("config: database.port is required for the %s driver", DriverPostgres)
		}
		if c.Database.User == "" {
			return fmt.Errorf("config: database.user is required for the %s driver", DriverPostgres)
		}
		if c.Database.Database == "" {
			return fmt.Errorf("config: database.database is required for the %s driver", DriverPostgres)
		}
		// A Postgres deployment with TLS off is a deployment mistake worth naming,
		// because the DSN travels credentials across the wire.
		if c.Database.SSLMode == "" || c.Database.SSLMode == "disable" {
			if c.Environment == "production" {
				return fmt.Errorf("config: database.sslmode must not be %q in production; "+
					"the DSN carries a credential", c.Database.SSLMode)
			}
		}
	case "":
		return fmt.Errorf("config: database.driver is required (use %q or %q)", DriverSQLite, DriverPostgres)
	default:
		return fmt.Errorf("config: unsupported database.driver %q", c.Database.Driver)
	}
	return nil
}

func (c *Config) validateCache() error {
	switch c.Cache.Driver {
	case DriverMemory:
		return nil
	case DriverRedis:
		if c.Cache.RedisAddress == "" {
			return fmt.Errorf("config: cache.redis_address is required for the %s driver", DriverRedis)
		}
		return nil
	case "":
		return fmt.Errorf("config: cache.driver is required (use %q or %q)", DriverMemory, DriverRedis)
	default:
		return fmt.Errorf("config: unsupported cache.driver %q", c.Cache.Driver)
	}
}

func (c *Config) validateDrive() error {
	// The narrow per-file scope and the app-data scope are the only scopes this
	// release requests. A broader scope in a config file is a stop-the-line
	// error, not a warning.
	for _, scope := range c.Drive.Scopes {
		if scope != "https://www.googleapis.com/auth/drive.file" &&
			scope != "https://www.googleapis.com/auth/drive.appdata" {
			return fmt.Errorf("config: drive.scopes may only contain drive.file and drive.appdata scopes in this release, got %q", scope)
		}
	}
	if c.Drive.PreflightAttempts < 1 {
		return fmt.Errorf("config: drive.preflight_attempts must be at least 1")
	}
	if c.Drive.PreflightTimeout <= 0 || c.Drive.RoutingBudget <= 0 {
		return fmt.Errorf("config: drive preflight timeouts must be positive")
	}
	// A single attempt must fit inside the total routing budget, otherwise the
	// budget silently truncates the retry loop the docs require.
	if time.Duration(c.Drive.PreflightAttempts)*c.Drive.PreflightTimeout.Duration() > c.Drive.RoutingBudget.Duration() {
		return fmt.Errorf("config: drive.preflight_attempts x preflight_timeout exceeds drive.routing_budget")
	}
	if c.Drive.CircuitFailures < 1 || c.Drive.CircuitCooldown <= 0 {
		return fmt.Errorf("config: drive circuit thresholds must be positive")
	}
	return nil
}

func (c *Config) validateWorker() error {
	if c.Worker.Lease.Duration() < 0 {
		return fmt.Errorf("config: worker.lease must be positive")
	}
	// A lease shorter than the poll interval means a worker can hold a job, go away
	// for a poll cycle, and have its work taken while it is still running.
	if c.Worker.Poll.Duration() > 0 && c.Worker.Lease.Duration() > 0 &&
		c.Worker.Poll.Duration() >= c.Worker.Lease.Duration() {
		return fmt.Errorf("config: worker.poll (%s) must be shorter than worker.lease (%s); "+
			"otherwise a job can be taken from a worker that is still running it",
			c.Worker.Poll.Duration(), c.Worker.Lease.Duration())
	}
	return nil
}

func (c *Config) validateFallback() error {
	if !c.Fallback.Enabled {
		return nil
	}
	if c.Fallback.ConsentRequired {
		return fmt.Errorf("config: fallback.consent_required must be false; Storvia refuses to enable outage uploads until the disclosure is accepted")
	}
	if len(c.Fallback.Backends) == 0 {
		return fmt.Errorf("config: fallback.enabled requires at least one backend")
	}
	seen := make(map[string]struct{}, len(c.Fallback.Backends))
	for _, b := range c.Fallback.Backends {
		if b.ID == "" {
			return fmt.Errorf("config: every fallback backend requires an id")
		}
		if _, dup := seen[b.ID]; dup {
			return fmt.Errorf("config: duplicate fallback backend id %q", b.ID)
		}
		seen[b.ID] = struct{}{}
		switch b.Vendor {
		case VendorCloudflareR2, VendorAWSS3:
		default:
			return fmt.Errorf("config: unsupported fallback vendor %q for backend %q", b.Vendor, b.ID)
		}
		if b.Disabled {
			continue
		}
		if b.Bucket == "" {
			return fmt.Errorf("config: fallback backend %q requires a bucket", b.ID)
		}
		if b.Endpoint != "" && !c.endpointAllowed(b.Endpoint) {
			return fmt.Errorf("config: fallback backend %q endpoint is not permitted by security.allowed_endpoint_schemes/hosts", b.ID)
		}
	}
	return nil
}

// hostAllowed reports whether a host matches one allow-list entry.
//
// An entry may begin with "*." to match any single leading label, so
// "*.r2.cloudflarestorage.com" permits a tenant's own account endpoint without the
// account identifier having to appear in the checked-in configuration. The wildcard
// is limited to one label deliberately: "*.r2.cloudflarestorage.com" matches
// "abc123.r2.cloudflarestorage.com" and nothing deeper, so it cannot be widened into
// a match for an unrelated host.
func hostAllowed(allowed, host string) bool {
	if strings.HasPrefix(allowed, "*.") {
		suffix := allowed[1:] // ".r2.cloudflarestorage.com"
		if !strings.HasSuffix(strings.ToLower(host), strings.ToLower(suffix)) {
			return false
		}
		label := host[:len(host)-len(suffix)]
		// Exactly one non-empty label before the suffix.
		return label != "" && !strings.Contains(label, ".")
	}
	return strings.EqualFold(allowed, host)
}

// endpointAllowed enforces the operator allow-list that stops a misconfigured or
// tampered endpoint from pointing Storvia at an internal or hostile host.
func (c *Config) endpointAllowed(endpoint string) bool {
	normalized := endpoint
	for _, scheme := range c.Security.AllowedEndpointSchemes {
		if strings.HasPrefix(normalized, scheme+"://") {
			normalized = strings.TrimPrefix(normalized, scheme+"://")
			goto schemeOk
		}
	}
	return false

schemeOk:
	if idx := strings.IndexAny(normalized, "/?#"); idx >= 0 {
		normalized = normalized[:idx]
	}
	if idx := strings.LastIndex(normalized, ":"); idx >= 0 && !strings.Contains(normalized[idx:], "]") {
		normalized = normalized[:idx]
	}
	normalized = strings.ToLower(normalized)
	if len(c.Security.AllowedEndpointHosts) == 0 {
		return true
	}
	for _, allowed := range c.Security.AllowedEndpointHosts {
		if hostAllowed(strings.TrimSpace(allowed), normalized) {
			return true
		}
	}
	return false
}

func (c *Config) validateUploads() error {
	if c.Uploads.MaxFileSizeBytes <= 0 {
		return fmt.Errorf("config: uploads.max_file_size_bytes must be positive")
	}
	if len(c.Uploads.AllowedMIMETypes) == 0 {
		return fmt.Errorf("config: uploads.allowed_mime_types must not be empty; Storvia does not accept arbitrary uploads")
	}
	if len(c.Uploads.AllowedCategories) == 0 {
		return fmt.Errorf("config: uploads.allowed_categories must not be empty")
	}
	for category, limit := range c.Uploads.CategoryLimits {
		if limit <= 0 {
			return fmt.Errorf("config: uploads.category_limits[%q] must be positive", category)
		}
		if limit > c.Uploads.MaxFileSizeBytes {
			return fmt.Errorf("config: uploads.category_limits[%q] exceeds uploads.max_file_size_bytes", category)
		}
	}
	// Drive requires chunks to be a multiple of 256 KiB.
	const driveChunkMultiple = 256 << 10
	if c.Uploads.DriveChunkSizeBytes%driveChunkMultiple != 0 {
		return fmt.Errorf("config: uploads.drive_chunk_size_bytes must be a multiple of %d", driveChunkMultiple)
	}
	if c.Uploads.S3PartSizeBytes < c.Uploads.MinPartSizeBytes {
		return fmt.Errorf("config: uploads.s3_part_size_bytes must be at least uploads.min_part_size_bytes")
	}
	return nil
}

// SecretRefs returns every environment reference that must resolve to a secret.
//
// It is the authoritative list, so startup validation, the redaction log line, and
// the tests all agree on what "secret" means instead of each keeping its own list.
func (c *Config) SecretRefs() []SecretRef {
	refs := []SecretRef{
		{Field: "security.encryption_key", EnvVar: EnvEncryptionKey, Required: true},
		{Field: "auth.session_signing_key", EnvVar: EnvSessionSigningKey, Required: true},
		{Field: "drive.client_id", EnvVar: EnvGoogleClientID, Required: false},
		{Field: "drive.client_secret", EnvVar: EnvGoogleClientSecret, Required: false},
		{Field: "security.encryption_key_previous", EnvVar: EnvEncryptionKeyPrevious, Required: false},
	}
	if c.Cache.Driver == DriverRedis {
		refs = append(refs, SecretRef{
			Field: "cache.redis_password", EnvVar: EnvRedisPassword, Required: false,
		})
	}
	if c.Database.Driver == DriverPostgres {
		refs = append(refs, SecretRef{
			Field: "database.password", EnvVar: EnvDatabasePassword, Required: true,
		})
	}
	for _, backend := range c.Fallback.Backends {
		if backend.Disabled {
			continue
		}
		// Credential variable names are chosen by the vendor, not by the backend
		// identifier, so a deployment cannot point an R2 backend at the AWS
		// credentials by mistake.
		idVar, secretVar, tokenVar := EnvR2AccessKeyID, EnvR2SecretAccessKey, EnvR2SessionToken
		if backend.Vendor == VendorAWSS3 {
			idVar, secretVar, tokenVar = EnvAWSAccessKeyID, EnvAWSSecretAccessKey, ""
		}
		refs = append(refs,
			SecretRef{Field: "fallback.backends[" + backend.ID + "].access_key_id",
				EnvVar: idVar, Required: true},
			SecretRef{Field: "fallback.backends[" + backend.ID + "].secret_access_key",
				EnvVar: secretVar, Required: true},
		)
		if tokenVar != "" {
			refs = append(refs, SecretRef{
				Field:  "fallback.backends[" + backend.ID + "].session_token",
				EnvVar: tokenVar, Required: false,
			})
		}
	}
	return refs
}

// SecretRef is one environment reference to a secret.
type SecretRef struct {
	// Field is the dotted configuration path, used in error messages.
	Field string
	// EnvVar is the environment variable name. It names the variable, never its
	// value.
	EnvVar string
	// Required is false for a reference that is only needed when the feature it
	// belongs to is switched on.
	Required bool
}

// ValidateSecretRefs rejects a reference that is not an environment variable name.
//
// This is the guard that keeps the boundary honest. A checked-in config file names no
// secret at all, so the references come from the constants in secrets_names.go rather
// than from configuration; this check is what stops a bad edit to that list, or a
// reference that somehow resolves to itself, from turning into a configuration that
// silently never authenticates.
func ValidateSecretRefs(refs []SecretRef) error {
	for _, ref := range refs {
		if ref.EnvVar == "" {
			if ref.Required {
				return fmt.Errorf("config: %s has no environment reference set", ref.Field)
			}
			continue
		}
		if !validEnvVarName(ref.EnvVar) {
			return fmt.Errorf("config: %s must name an environment variable, not a value; "+
				"got %q. Secrets belong in the environment, never in a JSON config file",
				ref.Field, redactIfCredential(ref.EnvVar))
		}
		if looksLikeCredential(ref.EnvVar) {
			return fmt.Errorf("config: %s appears to contain a credential rather than a "+
				"variable name. Secrets belong in the environment, never in a JSON config file",
				ref.Field)
		}
	}
	return nil
}

// validEnvVarName matches a conventional environment variable name.
func validEnvVarName(name string) bool {
	for i, r := range name {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return name != ""
}

// credentialPrefixes are recognizable starts of real credentials, used to catch a
// secret pasted where a variable name belongs.
var credentialPrefixes = []string{
	"AKIA", "ASIA", "ya29.", "sk-", "ghp_", "gho_", "xoxb-", "-----BEGIN",
}

// looksLikeCredential reports whether a value resembles a real secret.
//
// It is a heuristic and deliberately narrow: it would rather miss an unusual
// credential than reject a legitimate value. URLs, file paths, and scope
// identifiers are excluded, because a long string with no underscore is a very
// ordinary shape for those and flagging them would make the check useless.
func looksLikeCredential(value string) bool {
	// A scheme or a path separator is strong evidence this is a URL or a path,
	// not a credential. Real secrets do not carry either.
	if strings.Contains(value, "://") || strings.Contains(value, "/") {
		return false
	}
	for _, prefix := range credentialPrefixes {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	// A long opaque string with no separators is almost always a credential; an
	// environment variable name nearly always contains an underscore.
	if len(value) >= 40 && !strings.Contains(value, "_") {
		return true
	}
	return false
}

// redactIfCredential masks a value that looks like a credential so an error
// message can name the field without reprinting the secret.
func redactIfCredential(value string) string {
	if looksLikeCredential(value) {
		return "[REDACTED]"
	}
	return value
}

// ResolveSecrets resolves every reference through lookup.
//
// It fails when a required secret is absent. That failure matters: previously a
// missing secret produced an empty credential, which surfaced much later as an
// authentication error, or worse, as a silently disabled fallback backend.
func (c *Config) ResolveSecrets(lookup func(string) (string, bool)) (map[string]string, error) {
	if err := ValidateSecretRefs(c.SecretRefs()); err != nil {
		return nil, err
	}

	resolved := make(map[string]string, len(c.SecretRefs()))
	var missing []string
	for _, ref := range c.SecretRefs() {
		if ref.EnvVar == "" {
			continue
		}
		value, ok := lookup(ref.EnvVar)
		if !ok || value == "" {
			if ref.Required {
				missing = append(missing, fmt.Sprintf("%s (%s)", ref.Field, ref.EnvVar))
			}
			continue
		}
		resolved[ref.EnvVar] = value
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("config: required secret(s) not set in the environment: %s",
			strings.Join(missing, ", "))
	}
	return resolved, nil
}

// SecretFields returns the dotted paths of every field holding resolved secret
// material. Diagnostics print these paths with a redacted marker so an operator
// can confirm a key arrived without seeing its value.
func (c *Config) SecretFields() []string {
	out := make([]string, 0, len(c.SecretRefs()))
	for _, ref := range c.SecretRefs() {
		out = append(out, ref.Field)
	}
	return out
}

// RedactedSummary renders a startup summary with no secret values, used by the
// bootstrap log line.
func (c *Config) RedactedSummary() map[string]any {
	return map[string]any{
		"environment":      c.Environment,
		"service":          c.Service.Name,
		"release":          c.Service.Release,
		"listen":           c.Server.Addr(),
		"database_driver":  c.Database.Driver,
		"cache_driver":     c.Cache.Driver,
		"drive_scopes":     c.Drive.Scopes,
		"fallback_enabled": c.Fallback.Enabled,
		"fallback_vendors": c.FallbackActiveVendors(),
		"upload_max_bytes": c.Uploads.MaxFileSizeBytes,
		"routing_budget":   c.Drive.RoutingBudget.String(),
	}
}

// FallbackActiveVendors lists the vendors of backends eligible for outage use.
func (c *Config) FallbackActiveVendors() []string {
	out := make([]string, 0, len(c.Fallback.Backends))
	for _, b := range c.Fallback.ActiveBackends() {
		out = append(out, b.Vendor)
	}
	return out
}
