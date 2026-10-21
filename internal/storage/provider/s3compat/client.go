// Package s3compat implements the S3-compatible outage fallback.
//
// Scope note: this adapter targets the common S3 subset that both AWS S3 and
// Cloudflare R2 expose. It deliberately does not use object lock, versioning, ACL
// mutations, or tagging, and it reports capability flags rather than pretending
// those features are portable. One implementation of this code does not verify
// both vendors; each must be exercised against its own non-production bucket.
package s3compat

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/shovan04/storvia-project/internal/storage/model"
	"github.com/shovan04/storvia-project/internal/storage/policy"
)

// Credentials holds the resolved secret material for one backend. It is built
// from environment references at startup and never travels through the API.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// Config describes one S3-compatible backend.
type Config struct {
	ID     string
	Vendor string
	// Endpoint is empty for AWS S3, and the account endpoint for R2.
	Endpoint     string
	Region       string
	Bucket       string
	Prefix       string
	Credentials  Credentials
	UsePathStyle bool
	// PresignTTL bounds how long a capability URL handed to a browser lives.
	PresignTTL time.Duration
	// RetryAttempts is the SDK retry ceiling. It is deliberately small: the
	// coordinator owns the operation budget and must not be multiplied by an
	// SDK retry loop of its own.
	RetryAttempts int
}

// Validate rejects a configuration that would misdirect writes or leak.
func (c Config) Validate() error {
	if c.ID == "" {
		return errors.New("s3compat: backend id is required")
	}
	if c.Bucket == "" {
		return errors.New("s3compat: bucket is required")
	}
	if c.Credentials.AccessKeyID == "" || c.Credentials.SecretAccessKey == "" {
		return errors.New("s3compat: access key id and secret access key are required")
	}
	switch c.Vendor {
	case vendorR2, vendorS3:
	default:
		return fmt.Errorf("s3compat: unsupported vendor %q", c.Vendor)
	}
	if c.PresignTTL <= 0 {
		return errors.New("s3compat: presign ttl must be positive")
	}
	if c.RetryAttempts < 0 || c.RetryAttempts > 5 {
		return errors.New("s3compat: retry attempts must be between 0 and 5")
	}
	return nil
}

const (
	vendorR2 = "cloudflare_r2"
	vendorS3 = "aws_s3"
)

// DefaultR2Region is the region value R2 requires. It is not a real region.
const DefaultR2Region = "auto"

// Client is the S3-compatible provider.
type Client struct {
	api     *s3.Client
	presign *s3.PresignClient
	cfg     Config
	// multipartTTL is how long an abandoned multipart upload survives before
	// lifecycle rules may clean it up.
	multipartTTL time.Duration
}

var _ providerStorage = (*Client)(nil)

// providerStorage mirrors provider.Storage without importing it, keeping this
// package's compile-time assertion local and cheap. The real conformance check
// lives in the provider package test.
type providerStorage interface {
	Name() string
}

// Options tune client construction.
type Options struct {
	// RegionOverride wins over the config region, mainly for tests.
	RegionOverride string
	// MultipartTTL is the abandonment threshold reported to operators.
	MultipartTTL time.Duration
}

// New builds a client for one backend.
//
// The AWS SDK retryer is constrained to cfg.RetryAttempts so the SDK cannot
// create retries that multiply with the coordinator's own bounded loop. That
// multiplication is the documented failure mode this config prevents.
func New(ctx context.Context, cfg Config, opts Options) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Vendor == vendorR2 && cfg.Region == "" {
		cfg.Region = DefaultR2Region
	}
	if cfg.Vendor == vendorR2 && !cfg.UsePathStyle && cfg.Endpoint == "" {
		return nil, errors.New("s3compat: cloudflare_r2 requires an account endpoint")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}

	loadOpts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.Credentials.AccessKeyID,
			cfg.Credentials.SecretAccessKey,
			cfg.Credentials.SessionToken,
		)),
	}
	if opts.RegionOverride != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(opts.RegionOverride))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("s3compat: load aws config: %w", err)
	}

	s3Opts := []func(*s3.Options){
		func(o *s3.Options) {
			// A bounded SDK retry ceiling. The coordinator budget is the
			// authority; the SDK must stay well inside it.
			o.RetryMaxAttempts = cfg.RetryAttempts
		},
	}
	if cfg.Endpoint != "" {
		endpoint := cfg.Endpoint
		pathStyle := cfg.UsePathStyle
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.BaseEndpoint = &endpoint
			// Path style keeps the bucket in the path, which is what R2 and most
			// S3-compatible services require.
			o.UsePathStyle = pathStyle
		})
	}

	api := s3.NewFromConfig(awsCfg, s3Opts...)
	return &Client{
		api:          api,
		presign:      s3.NewPresignClient(api),
		cfg:          cfg,
		multipartTTL: opts.MultipartTTL,
	}, nil
}

// Name implements provider.Storage.
func (c *Client) Name() string { return string(model.ProviderS3Compatible) }

// Vendor returns the configured vendor.
func (c *Client) Vendor() string { return c.cfg.Vendor }

// Bucket returns the configured bucket.
func (c *Client) Bucket() string { return c.cfg.Bucket }

// MultipartTTL returns the configured abandonment threshold.
func (c *Client) MultipartTTL() time.Duration { return c.multipartTTL }

// BuildKey generates the object key for a logical object.
//
// The key is derived server-side from the opaque object ID and the attempt
// generation. It never incorporates a user-supplied name, and a client can never
// influence it, which is what prevents one tenant from addressing another's
// object or choosing where their own bytes land.
func (c *Client) BuildKey(tenantID, objectID string, generation int) string {
	segments := []string{"storvia", "tenants", sanitizeSegment(tenantID), "objects", sanitizeSegment(objectID)}
	if generation > 0 {
		segments = append(segments, "g"+strconv.Itoa(generation))
	}
	return strings.Join(segments, "/")
}

// sanitizeSegment keeps a generated key segment to characters that need no
// escaping, so a key never contains a path traversal or a delimiter injection.
func sanitizeSegment(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// WithPrefix returns key under the backend's configured prefix.
func (c *Client) WithPrefix(key string) string {
	prefix := strings.Trim(c.cfg.Prefix, "/")
	if prefix == "" {
		return key
	}
	return prefix + "/" + key
}

// OwnsKey reports whether key lives under this backend's Storvia prefix. It is
// the guard for orphan sweeps: a sweep must never consider an object it did not
// generate a candidate for deletion.
//
// The configured prefix is stripped before the namespace check, because a key in
// the bucket always carries the prefix while the Storvia namespace begins after
// it. Checking the raw key against the namespace would reject Storvia's own keys
// whenever a prefix is configured.
func (c *Client) OwnsKey(key string) bool {
	prefix := strings.Trim(c.cfg.Prefix, "/")
	if prefix != "" {
		if !strings.HasPrefix(key, prefix+"/") {
			return false
		}
		key = strings.TrimPrefix(key, prefix+"/")
	}
	return strings.HasPrefix(key, "storvia/tenants/")
}

// Availability performs a cheap read-only reachability check.
//
// It uses ListObjectsV2 with MaxKeys 1 against the Storvia prefix, which
// exercises the endpoint, the signature, and the credentials without reading or
// writing object bytes. A failure here is reported as a configuration or
// availability class; it never causes a switch back to Drive.
func (c *Client) Availability(ctx context.Context, scope model.AvailabilityScope) error {
	_, err := c.api.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:  awsString(c.cfg.Bucket),
		Prefix:  awsString(c.prefix()),
		MaxKeys: awsInt32(1),
	})
	if err != nil {
		return c.normalize(ctx, "Availability", err, false)
	}
	return nil
}

// Head verifies an object exists and returns its size, content type, and
// checksum. The service compares these with the expected values before it
// commits a ready location.
func (c *Client) Head(ctx context.Context, object model.ProviderObject) (model.ObjectMetadata, error) {
	bucket, key := c.resolve(object)
	out, err := c.api.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: awsString(bucket),
		Key:    awsString(key),
	})
	if err != nil {
		return model.ObjectMetadata{}, c.normalize(ctx, "Head", err, false)
	}

	meta := model.ObjectMetadata{
		OriginalName: object.FileID,
		MIMEType:     aws.ToString(out.ContentType),
		SizeBytes:    aws.ToInt64(out.ContentLength),
	}
	// Prefer a real checksum when the provider supplies one; fall back to the
	// ETag, which for a single-part upload is the MD5 of the body.
	switch {
	case out.ChecksumCRC32 != nil && *out.ChecksumCRC32 != "":
		meta.ChecksumAlgorithm = "crc32c"
		meta.Checksum = *out.ChecksumCRC32
	case out.ChecksumSHA256 != nil && *out.ChecksumSHA256 != "":
		meta.ChecksumAlgorithm = "sha256"
		meta.Checksum = *out.ChecksumSHA256
	case out.ETag != nil && *out.ETag != "":
		meta.ChecksumAlgorithm = "etag"
		meta.Checksum = strings.Trim(*out.ETag, `"`)
	}
	return meta, nil
}

// Put writes the whole object in one call. It is used by the backend-proxy path
// and by tests; the client-facing path uses presigned parts instead.
func (c *Client) Put(ctx context.Context, object model.ProviderObject, body io.Reader, size int64, mimeType string) (model.ProviderObject, error) {
	bucket, key := c.resolve(object)
	input := &s3.PutObjectInput{
		Bucket:        awsString(bucket),
		Key:           awsString(key),
		Body:          body,
		ContentLength: awsInt64(size),
	}
	if mimeType != "" {
		input.ContentType = awsString(mimeType)
	}
	out, err := c.api.PutObject(ctx, input)
	if err != nil {
		return model.ProviderObject{}, c.normalize(ctx, "Put", err, true)
	}
	return model.ProviderObject{
		Provider:  model.ProviderS3Compatible,
		ObjectKey: key,
		Bucket:    bucket,
		SizeBytes: size,
		ETag:      strings.Trim(aws.ToString(out.ETag), `"`),
	}, nil
}

// Get streams an object or a byte range of it.
//
// The caller authorizes the application user first; this function is only ever
// reached with an already-authorized, tenant-scoped object.
func (c *Client) Get(ctx context.Context, object model.ProviderObject, byteRange *model.ByteRange) (io.ReadCloser, int64, string, error) {
	bucket, key := c.resolve(object)
	input := &s3.GetObjectInput{Bucket: awsString(bucket), Key: awsString(key)}
	if header := byteRange.String(); header != "" {
		input.Range = awsString(header)
	}
	out, err := c.api.GetObject(ctx, input)
	if err != nil {
		return nil, 0, "", c.normalize(ctx, "Get", err, false)
	}
	size := int64(-1)
	if out.ContentLength != nil {
		size = *out.ContentLength
	}
	return out.Body, size, aws.ToString(out.ContentType), nil
}

// Delete removes the object from its recorded location.
//
// A not-found result is returned as a ClassMissing error rather than success, so
// the service can distinguish "we deleted it" from "it was already gone" and
// apply its own already-absent policy. It never deletes a same-named object on
// another provider.
func (c *Client) Delete(ctx context.Context, object model.ProviderObject) error {
	bucket, key := c.resolve(object)
	_, err := c.api.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: awsString(bucket),
		Key:    awsString(key),
	})
	if err != nil {
		return c.normalize(ctx, "Delete", err, true)
	}
	return nil
}

// CreateMultipartUpload starts a multipart upload for a generated key.
func (c *Client) CreateMultipartUpload(ctx context.Context, object model.ProviderObject, mimeType string) (string, error) {
	bucket, key := c.resolve(object)
	input := &s3.CreateMultipartUploadInput{Bucket: awsString(bucket), Key: awsString(key)}
	if mimeType != "" {
		input.ContentType = awsString(mimeType)
	}
	out, err := c.api.CreateMultipartUpload(ctx, input)
	if err != nil {
		return "", c.normalize(ctx, "CreateMultipartUpload", err, true)
	}
	return aws.ToString(out.UploadId), nil
}

// CompleteMultipartUpload finalizes a multipart upload.
//
// The caller retries this after a lost response, which is safe because
// CompleteMultipartUpload is idempotent for a given upload ID and part list.
func (c *Client) CompleteMultipartUpload(ctx context.Context, object model.ProviderObject, uploadID string, parts []CompletedPart) error {
	bucket, key := c.resolve(object)
	completed := make([]types.CompletedPart, 0, len(parts))
	for _, part := range parts {
		completed = append(completed, types.CompletedPart{
			ETag:       awsString(part.ETag),
			PartNumber: awsInt32(int32(part.PartNumber)),
		})
	}
	_, err := c.api.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          awsString(bucket),
		Key:             awsString(key),
		UploadId:        awsString(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	})
	if err != nil {
		return c.normalize(ctx, "CompleteMultipartUpload", err, true)
	}
	return nil
}

// AbortMultipartUpload discards an in-flight multipart upload during cleanup.
func (c *Client) AbortMultipartUpload(ctx context.Context, object model.ProviderObject, uploadID string) error {
	bucket, key := c.resolve(object)
	_, err := c.api.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   awsString(bucket),
		Key:      awsString(key),
		UploadId: awsString(uploadID),
	})
	if err != nil {
		normalized := c.normalize(ctx, "AbortMultipartUpload", err, true)
		// An already-gone session is the desired end state for an abort.
		if normalized.Class == policy.ClassMissing {
			return nil
		}
		return normalized
	}
	return nil
}

// CompletedPart is one uploaded part, in part-number order.
type CompletedPart struct {
	PartNumber int
	ETag       string
}

// InspectUpload reports whether a multipart upload is still in flight.
//
// It answers the "did the provider accept my write?" question for a specific key
// and upload ID only. It never probes another key and never creates anything.
func (c *Client) InspectUpload(ctx context.Context, object model.ProviderObject, uploadID string) (model.UploadState, error) {
	bucket, key := c.resolve(object)
	out, err := c.api.ListParts(ctx, &s3.ListPartsInput{
		Bucket:   awsString(bucket),
		Key:      awsString(key),
		UploadId: awsString(uploadID),
		MaxParts: awsInt32(1000),
	})
	if err != nil {
		normalized := c.normalize(ctx, "InspectUpload", err, false)
		if normalized.Class == policy.ClassMissing {
			// No such multipart upload: the session expired or was aborted, so
			// there is nothing in flight on this key.
			return model.UploadState{Exists: false, Expired: true}, nil
		}
		return model.UploadState{}, normalized
	}

	state := model.UploadState{Exists: true}
	var offset int64
	for _, part := range out.Parts {
		if part.Size != nil {
			offset += *part.Size
		}
	}
	state.ConfirmedOffset = offset

	// A completed upload leaves no resumable parts behind but leaves the object.
	head, headErr := c.api.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: awsString(bucket),
		Key:    awsString(key),
	})
	if headErr == nil {
		state.Completed = true
		state.ConfirmedOffset = aws.ToInt64(head.ContentLength)
	}
	return state, nil
}

// PresignedPart is one short-lived upload capability.
//
// The URL is a bearer token. It is returned only to the authorized tenant, over
// the transfer step that needs it, and is never written to a log or stored in a
// database row.
type PresignedPart struct {
	PartNumber int
	URL        string
	// Headers must be replayed verbatim by the client or the signature fails.
	Headers   map[string][]string
	ExpiresAt time.Time
}

// PlanPresignedUpload returns a bounded set of presigned PUT URLs for a new
// upload. It is used only after the policy has already selected the fallback, so
// a client can never request S3 directly.
func (c *Client) PlanPresignedUpload(ctx context.Context, object model.ProviderObject, partCount int) ([]PresignedPart, error) {
	if partCount < 1 {
		return nil, errors.New("s3compat: part count must be positive")
	}
	bucket, key := c.resolve(object)
	parts := make([]PresignedPart, 0, partCount)
	for i := 1; i <= partCount; i++ {
		req, err := c.presign.PresignPutObject(ctx, &s3.PutObjectInput{
			Bucket: awsString(bucket),
			Key:    awsString(key),
		}, s3.WithPresignExpires(c.cfg.PresignTTL))
		if err != nil {
			return nil, fmt.Errorf("s3compat: presign part %d: %w", i, err)
		}
		parts = append(parts, PresignedPart{
			PartNumber: i,
			URL:        req.URL,
			Headers:    req.SignedHeader,
			ExpiresAt:  time.Now().Add(c.cfg.PresignTTL),
		})
	}
	return parts, nil
}

// PresignGet returns a short-lived download capability.
//
// It exists for the narrow case where the privacy model explicitly allows a
// direct browser download. The default path proxies the stream instead, so that
// no capability URL escapes the API.
func (c *Client) PresignGet(ctx context.Context, object model.ProviderObject, ttl time.Duration) (string, error) {
	if ttl <= 0 || ttl > c.cfg.PresignTTL {
		return "", errors.New("s3compat: presign ttl exceeds the configured maximum")
	}
	bucket, key := c.resolve(object)
	req, err := c.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: awsString(bucket),
		Key:    awsString(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("s3compat: presign get: %w", err)
	}
	return req.URL, nil
}

// ListStorviaPrefix returns keys under the Storvia prefix for orphan detection.
//
// Listing is cleanup evidence, never permission to recreate an object. The
// caller applies a grace period and an audit record before deleting anything,
// because a delayed database commit can make a valid object look orphaned.
func (c *Client) ListStorviaPrefix(ctx context.Context, continuationToken string, maxKeys int32) ([]string, error) {
	if maxKeys <= 0 || maxKeys > 1000 {
		maxKeys = 1000
	}
	input := &s3.ListObjectsV2Input{
		Bucket:  awsString(c.cfg.Bucket),
		Prefix:  awsString(c.prefix()),
		MaxKeys: awsInt32(maxKeys),
	}
	if continuationToken != "" {
		input.ContinuationToken = awsString(continuationToken)
	}
	out, err := c.api.ListObjectsV2(ctx, input)
	if err != nil {
		return nil, c.normalize(ctx, "ListOrphans", err, false)
	}
	keys := make([]string, 0, len(out.Contents))
	for _, item := range out.Contents {
		if key := aws.ToString(item.Key); c.OwnsKey(key) {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

// prefix returns the prefix Storvia owns inside the bucket.
func (c *Client) prefix() string {
	prefix := strings.Trim(c.cfg.Prefix, "/")
	if prefix == "" {
		return "storvia/"
	}
	return prefix + "/"
}

// resolve picks the bucket and key for an object, applying the prefix. The
// client can supply neither: the key comes from the recorded object row.
func (c *Client) resolve(object model.ProviderObject) (bucket, key string) {
	bucket = object.Bucket
	if bucket == "" {
		bucket = c.cfg.Bucket
	}
	return bucket, c.WithPrefix(object.ObjectKey)
}

// normalize converts an SDK error into a policy.ProviderError.
//
// This is the only place an S3 error becomes a routing input, so the mapping is
// kept in one function and covered directly by tests.
func (c *Client) normalize(ctx context.Context, operation string, err error, mutationMayHaveStarted bool) *policy.ProviderError {
	status, code := classifySDKError(err)

	class := policy.ClassifyS3Error(status, code)
	// A deadline or cancelled context after a mutation may have been sent is
	// uncertainty, not failure. Collapsing it to "unavailable" here would make
	// an ambiguous write look like an eligible outage.
	if isTimeout(err) && mutationMayHaveStarted {
		class = policy.ClassUncertain
	}
	// An absent status means the request never produced a response. Before a
	// mutation that is a transport outage; during one it is uncertain.
	if status == 0 && mutationMayHaveStarted && isTimeout(err) {
		class = policy.ClassUncertain
	}

	return &policy.ProviderError{
		Provider:               c.Name(),
		Operation:              operation,
		Class:                  class,
		StatusCode:             status,
		Reason:                 code,
		Retryable:              class == policy.ClassUnavailable || class == policy.ClassRateLimited,
		MutationMayHaveStarted: mutationMayHaveStarted,
		SafeDetail:             fmt.Sprintf("s3 operation %s failed with class %s", operation, class),
		Err:                    err,
	}
}

// classifySDKError pulls the HTTP status and S3 error code out of an SDK error.
func classifySDKError(err error) (status int, code string) {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code = apiErr.ErrorCode()
		var respErr interface{ HTTPStatusCode() int }
		if errors.As(err, &respErr) {
			status = respErr.HTTPStatusCode()
		}
	}
	if status == 0 {
		status = httpStatusFromError(err)
	}
	if code == "" && errors.Is(err, context.DeadlineExceeded) {
		code = "RequestTimeout"
	}
	return status, code
}

// httpStatusFromError digs a status out of the smithy response error chain.
func httpStatusFromError(err error) int {
	var respErr interface{ HTTPStatusCode() int }
	if errors.As(err, &respErr) {
		return respErr.HTTPStatusCode()
	}
	return 0
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr interface{ Timeout() bool }
	return errors.As(err, &netErr) && netErr.Timeout()
}

// ComputeSHA256Base64 returns the base64 SHA-256 of the payload, the checksum
// form both S3 and R2 accept for verification before a location is committed.
func ComputeSHA256Base64(data []byte) string {
	sum := sha256.Sum256(data)
	return base64.StdEncoding.EncodeToString(sum[:])
}
