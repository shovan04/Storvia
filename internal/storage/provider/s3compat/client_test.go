package s3compat

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/shovan04/storvia-project/internal/storage/model"
	"github.com/shovan04/storvia-project/internal/storage/policy"
)

// validConfig returns a configuration that passes validation.
func validConfig() Config {
	return Config{
		ID:            "r2-primary",
		Vendor:        vendorR2,
		Endpoint:      "https://account.r2.cloudflarestorage.com",
		Region:        "auto",
		Bucket:        "storvia-fallback",
		Prefix:        "prod",
		PresignTTL:    15 * time.Minute,
		RetryAttempts: 2,
		Credentials: Credentials{
			AccessKeyID:     "AKIAREALKEY",
			SecretAccessKey: "secret",
		},
	}
}

// TestConfigValidationRejectsUnsafeSettings covers the settings that would
// misdirect writes or leak credentials.
func TestConfigValidationRejectsUnsafeSettings(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"missing id", func(c *Config) { c.ID = "" }},
		{"missing bucket", func(c *Config) { c.Bucket = "" }},
		{"missing access key", func(c *Config) { c.Credentials.AccessKeyID = "" }},
		{"missing secret key", func(c *Config) { c.Credentials.SecretAccessKey = "" }},
		{"unknown vendor", func(c *Config) { c.Vendor = "minio" }},
		{"zero presign ttl", func(c *Config) { c.PresignTTL = 0 }},
		{"negative presign ttl", func(c *Config) { c.PresignTTL = -time.Minute }},
		// An unbounded SDK retryer multiplied by the coordinator loop is the
		// retry-storm failure mode.
		{"too many retry attempts", func(c *Config) { c.RetryAttempts = 20 }},
		{"negative retry attempts", func(c *Config) { c.RetryAttempts = -1 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Error("want an error")
			}
		})
	}

	cfg := validConfig()
	if err := cfg.Validate(); err != nil {
		t.Errorf("the baseline configuration must be valid: %v", err)
	}
}

// TestNewDefaultsR2Region checks the R2 region default, which is a literal
// sentinel rather than a real region.
func TestNewDefaultsR2Region(t *testing.T) {
	cfg := validConfig()
	cfg.Region = ""
	cfg.UsePathStyle = true

	client, err := New(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if client.cfg.Region != DefaultR2Region {
		t.Errorf("region = %q, want %q", client.cfg.Region, DefaultR2Region)
	}
	if DefaultR2Region != "auto" {
		t.Errorf("DefaultR2Region = %q, want auto", DefaultR2Region)
	}
}

func TestNewRejectsR2WithoutAnEndpoint(t *testing.T) {
	cfg := validConfig()
	cfg.Endpoint = ""
	cfg.UsePathStyle = false

	if _, err := New(context.Background(), cfg, Options{}); err == nil {
		t.Error("an R2 client without an account endpoint must be rejected")
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	cfg := validConfig()
	cfg.Bucket = ""
	if _, err := New(context.Background(), cfg, Options{}); err == nil {
		t.Error("New must surface a validation failure")
	}
}

// TestBuildKeyIsServerDerivedAndTenantScoped is the check that a client cannot
// choose where its bytes land.
func TestBuildKeyIsServerDerivedAndTenantScoped(t *testing.T) {
	client := &Client{cfg: validConfig()}

	key := client.BuildKey("tenant-a", "obj_123", 0)
	if !strings.HasPrefix(key, "storvia/tenants/tenant-a/objects/obj_123") {
		t.Errorf("key = %q, want the Storvia tenant prefix", key)
	}

	// Two tenants must not be able to collide.
	other := client.BuildKey("tenant-b", "obj_123", 0)
	if other == key {
		t.Error("two tenants produced the same key")
	}

	// A generation makes a retry land on a distinct key rather than overwriting.
	next := client.BuildKey("tenant-a", "obj_123", 2)
	if next == key {
		t.Error("a new generation produced the same key")
	}
	if !strings.Contains(next, "g2") {
		t.Errorf("key = %q, want the generation segment", next)
	}
}

// TestBuildKeySanitizesSegments prevents a path traversal or delimiter injection
// through a crafted tenant or object identifier.
func TestBuildKeySanitizesSegments(t *testing.T) {
	client := &Client{cfg: validConfig()}

	key := client.BuildKey("../../etc", "obj/../secret", 0)
	if strings.Contains(key, "..") {
		t.Errorf("key = %q, must not contain a traversal sequence", key)
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == "" || segment == "." || segment == ".." {
			t.Errorf("key %q contains an empty or relative segment", key)
		}
	}
}

func TestWithPrefixAndOwnsKey(t *testing.T) {
	client := &Client{cfg: validConfig()}

	key := client.BuildKey("tenant-a", "obj_1", 0)
	full := client.WithPrefix(key)
	if !strings.HasPrefix(full, "prod/") {
		t.Errorf("key = %q, want the configured prefix", full)
	}
	if !client.OwnsKey(full) {
		t.Error("OwnsKey must accept a key this client generated")
	}

	// A key outside the Storvia namespace must never be a cleanup candidate, or
	// an orphan sweep could delete unrelated objects.
	foreign := []string{
		"other/prefix/obj",
		"prod/someone-elses-data.txt",
		"random/file",
	}
	for _, key := range foreign {
		if client.OwnsKey(key) {
			t.Errorf("OwnsKey(%q) = true; only Storvia-generated keys may be swept", key)
		}
	}
}

func TestOwnsKeyWithoutAPrefix(t *testing.T) {
	cfg := validConfig()
	cfg.Prefix = ""
	client := &Client{cfg: cfg}

	key := client.BuildKey("tenant-a", "obj_1", 0)
	if client.WithPrefix(key) != key {
		t.Error("WithPrefix must be a no-op with an empty prefix")
	}
	if !client.OwnsKey(key) {
		t.Error("OwnsKey must accept a Storvia key when no prefix is configured")
	}
}

func TestResolveUsesTheRecordedKeyAndConfiguredBucket(t *testing.T) {
	cfg := validConfig()
	client := &Client{cfg: cfg}

	object := model.ProviderObject{
		Provider:  model.ProviderS3Compatible,
		ObjectKey: "storvia/tenants/tenant-a/objects/obj_1",
	}
	bucket, key := client.resolve(object)

	if bucket != "storvia-fallback" {
		t.Errorf("bucket = %q, want the configured bucket", bucket)
	}
	if key != "prod/storvia/tenants/tenant-a/objects/obj_1" {
		t.Errorf("key = %q, want the prefixed recorded key", key)
	}

	// An explicitly recorded bucket wins, so a multi-backend deployment can serve
	// an object from the bucket it was written to.
	object.Bucket = "other-bucket"
	if bucket, _ = client.resolve(object); bucket != "other-bucket" {
		t.Errorf("bucket = %q, want the recorded bucket", bucket)
	}
}

func TestClassifySDKErrorExtractsCodeAndStatus(t *testing.T) {
	// A plain context deadline carries no API error; the helper must still return
	// a zero status so the caller falls through to the transport classification.
	status, code := classifySDKError(context.DeadlineExceeded)
	if status != 0 {
		t.Errorf("status = %d, want 0 for a non-API error", status)
	}
	if code != "RequestTimeout" {
		t.Errorf("code = %q, want RequestTimeout", code)
	}
}

// TestNormalizeTurnsPostMutationTimeoutIntoUncertainty is the single most
// important classification rule on this provider.
func TestNormalizeTurnsPostMutationTimeoutIntoUncertainty(t *testing.T) {
	client := &Client{cfg: validConfig()}
	ctx := context.Background()

	// Before a mutation, a timeout is genuine unavailability.
	before := client.normalize(ctx, "Put", context.DeadlineExceeded, false)
	if before.Class != policy.ClassUnavailable {
		t.Errorf("pre-mutation timeout class = %q, want unavailable", before.Class)
	}

	// After a mutation may have been sent, the same timeout is uncertainty. A
	// timeout is never proof that a write failed.
	after := client.normalize(ctx, "Put", context.DeadlineExceeded, true)
	if after.Class != policy.ClassUncertain {
		t.Errorf("post-mutation timeout class = %q, want uncertain", after.Class)
	}
	if !after.MutationMayHaveStarted {
		t.Error("the mutation flag must be preserved")
	}
	if after.Retryable {
		t.Error("an uncertain result must not advertise itself as safely retryable")
	}
}

// TestNormalizeNeverMarksConfigurationFailuresAsOutages keeps a credential or
// endpoint problem from being routed around.
func TestNormalizeNeverMarksConfigurationFailuresAsOutages(t *testing.T) {
	client := &Client{cfg: validConfig()}
	ctx := context.Background()

	tests := []struct {
		name   string
		status int
		code   string
		want   policy.FailureClass
	}{
		{"signature mismatch", 403, "SignatureDoesNotMatch", policy.ClassConfiguration},
		{"invalid access key", 403, "InvalidAccessKeyId", policy.ClassConfiguration},
		{"missing key", 404, "NoSuchKey", policy.ClassMissing},
		{"slow down", 503, "SlowDown", policy.ClassRateLimited},
		{"quota", 400, "QuotaExceeded", policy.ClassQuota},
		{"internal", 500, "InternalError", policy.ClassUnavailable},
		{"invalid part", 400, "InvalidPart", policy.ClassValidation},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyLike(tc.status, tc.code)
			got := client.normalize(ctx, "Head", err, false)
			if got.Class != tc.want {
				t.Errorf("class = %q, want %q", got.Class, tc.want)
			}
		})
	}
}

// TestNormalizedErrorNeverCarriesRawPayload is the leak check on the error path.
func TestNormalizedErrorNeverCarriesRawPayload(t *testing.T) {
	client := &Client{cfg: validConfig()}

	err := client.normalize(context.Background(), "Get", context.DeadlineExceeded, false)
	if err.SafeDetail == "" {
		t.Error("SafeDetail must always be set")
	}
	// The Error string includes the wrapped cause, which the HTTP layer logs but
	// never returns; the client-facing field is SafeDetail.
	if strings.Contains(err.SafeDetail, "http://") {
		t.Errorf("SafeDetail leaked a URL: %q", err.SafeDetail)
	}
}

func TestProviderErrorStringIsSafeToLog(t *testing.T) {
	err := &policy.ProviderError{
		Provider:   "s3_compatible",
		Operation:  "Head",
		Class:      policy.ClassMissing,
		SafeDetail: "object not present",
	}
	text := err.Error()
	for _, expected := range []string{"s3_compatible", "Head", "missing"} {
		if !strings.Contains(text, expected) {
			t.Errorf("Error() = %q, want it to mention %q", text, expected)
		}
	}
}

func TestPresignedPartCountMustBePositive(t *testing.T) {
	client := &Client{cfg: validConfig()}
	if _, err := client.PlanPresignedUpload(context.Background(), model.ProviderObject{}, 0); err == nil {
		t.Error("a zero part count must be rejected")
	}
	if _, err := client.PlanPresignedUpload(context.Background(), model.ProviderObject{}, -1); err == nil {
		t.Error("a negative part count must be rejected")
	}
}

func TestComputeSHA256Base64(t *testing.T) {
	// Known vector: SHA-256 of the empty string, base64 encoded.
	got := ComputeSHA256Base64(nil)
	want := "47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU="
	if got != want {
		t.Errorf("ComputeSHA256Base64(nil) = %q, want %q", got, want)
	}
	// A non-empty input must differ from the empty one.
	if ComputeSHA256Base64([]byte("data")) == got {
		t.Error("different inputs produced the same checksum")
	}
}

func TestMultipartTTLIsReported(t *testing.T) {
	client := &Client{cfg: validConfig(), multipartTTL: 24 * time.Hour}
	if client.MultipartTTL() != 24*time.Hour {
		t.Errorf("MultipartTTL = %v, want 24h", client.MultipartTTL())
	}
}

// classifyLike produces a minimal API error carrying a code and status.
func classifyLike(status int, code string) error {
	return &statusError{apiErrorStub: &apiErrorStub{status: status, code: code, msg: "stub"}}
}
