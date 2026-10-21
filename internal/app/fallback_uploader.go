package app

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/shovan04/storvia-project/internal/storage/model"
	"github.com/shovan04/storvia-project/internal/storage/provider/s3compat"
	"github.com/shovan04/storvia-project/internal/storage/service"
)

// fallbackUploader adapts the S3-compatible clients to the service's narrow port.
//
// The service never sees an SDK type. That keeps the routing and completion logic
// testable with a stub and means a change to the S3 client cannot reach into the
// coordinator.
type fallbackUploader struct {
	backends map[string]*s3compat.Client
	// partCount is derived from the declared size and the configured part size.
	partSize   int64
	chunkSize  int
	presignTTL time.Duration
}

var _ service.FallbackUploader = (*fallbackUploader)(nil)

// newFallbackUploader builds the adapter. It returns nil when no backend is
// configured, which is the signal for the service to report that fallback transfer
// is unavailable rather than half-working.
func newFallbackUploader(backends map[string]*s3compat.Client, partSize int64, presignTTL time.Duration) service.FallbackUploader {
	if len(backends) == 0 {
		return nil
	}
	if partSize <= 0 {
		partSize = 8 << 20
	}
	if presignTTL <= 0 {
		presignTTL = 15 * time.Minute
	}
	return &fallbackUploader{backends: backends, partSize: partSize, presignTTL: presignTTL}
}

// PlanUpload returns one capability per part.
//
// The part count follows S3's limit of ten thousand parts, and the final part is
// whatever remains. For a small object this is a single presigned PUT, which keeps
// the common case to one round trip.
func (u *fallbackUploader) PlanUpload(ctx context.Context, backendID, objectKey string, sizeBytes int64) ([]service.PresignedPartInfo, error) {
	client, ok := u.backends[backendID]
	if !ok {
		return nil, fmt.Errorf("app: fallback backend %q is not configured", backendID)
	}

	partCount := 1
	if sizeBytes > u.partSize {
		partCount = int(math.Ceil(float64(sizeBytes) / float64(u.partSize)))
	}
	if partCount > 10000 {
		return nil, fmt.Errorf("app: %d bytes needs %d parts, above the S3 maximum of 10000", sizeBytes, partCount)
	}

	planned, err := client.PlanPresignedUpload(ctx, model.ProviderObject{ObjectKey: objectKey}, partCount)
	if err != nil {
		return nil, err
	}

	// Each capability carries the byte range it covers. The client cannot derive
	// these: it does not know the part size chosen here, and S3 rejects a part whose
	// boundaries differ from the ones the upload was created with. Returning them is
	// what makes the multipart contract usable at all.
	out := make([]service.PresignedPartInfo, 0, len(planned))
	for i, part := range planned {
		start := int64(i) * int64(u.partSize)
		end := start + int64(u.partSize) - 1
		if end > sizeBytes-1 {
			end = sizeBytes - 1
		}
		out = append(out, service.PresignedPartInfo{
			PartNumber: part.PartNumber,
			URL:        part.URL,
			Headers:    part.Headers,
			ExpiresAt:  part.ExpiresAt,
			StartByte:  start,
			EndByte:    end,
		})
	}
	return out, nil
}

// Verify confirms the object exists and reports its size.
func (u *fallbackUploader) Verify(ctx context.Context, backendID, objectKey string) (service.VerifiedObject, error) {
	client, ok := u.backends[backendID]
	if !ok {
		return service.VerifiedObject{}, fmt.Errorf("app: fallback backend %q is not configured", backendID)
	}

	object := model.ProviderObject{
		Provider:  model.ProviderS3Compatible,
		ObjectKey: objectKey,
		Bucket:    client.Bucket(),
	}
	meta, err := client.Head(ctx, object)
	if err != nil {
		return service.VerifiedObject{}, err
	}

	return service.VerifiedObject{
		SizeBytes: meta.SizeBytes,
		MIMEType:  meta.MIMEType,
		Checksum:  meta.Checksum,
	}, nil
}

// StorageStatus is exported for the admin panel's testing view.
var _ = strings.TrimSpace
