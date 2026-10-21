package app

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"github.com/shovan04/storvia-project/internal/apperr"
	"github.com/shovan04/storvia-project/internal/persistence"
	"github.com/shovan04/storvia-project/internal/storage/model"
	"github.com/shovan04/storvia-project/internal/storage/policy"
	"github.com/shovan04/storvia-project/internal/storage/provider/drive"
	"github.com/shovan04/storvia-project/internal/storage/provider/s3compat"
)

// contentSource resolves an object's recorded provider location.
//
// This is the component that enforces the no-copies rule on reads and deletes.
// There is deliberately no "try the other provider" branch: if the recorded
// provider is unavailable, the request fails, because Storvia has no second copy
// and a same-named object on the other provider is not this file.
type contentSource struct {
	backends map[string]*s3compat.Client
	drive    *drive.Client
	logger   *slog.Logger
}

// newContentSource builds the resolver.
func newContentSource(_ any, backends map[string]*s3compat.Client, driveClient *drive.Client, logger *slog.Logger) *contentSource {
	return &contentSource{backends: backends, drive: driveClient, logger: logger}
}

// Open streams an object from its recorded provider.
func (c *contentSource) Open(ctx context.Context, object model.LogicalObject, byteRange *model.ByteRange) (io.ReadCloser, error) {
	switch object.Provider {
	case model.ProviderS3Compatible:
		if object.BackendID == nil {
			return nil, c.locationFault(object, "no fallback backend is recorded")
		}
		client, ok := c.backends[*object.BackendID]
		if !ok {
			return nil, c.locationFault(object, "the recorded fallback backend is not configured on this deployment")
		}
		body, _, _, err := client.Get(ctx, providerObject(object, client), byteRange)
		if err != nil {
			return nil, err
		}
		return body, nil

	case model.ProviderGoogleDrive:
		fileID, err := c.driveFileID(object)
		if err != nil {
			return nil, err
		}
		body, _, _, err := c.drive.DownloadFile(ctx, c.scopeFor(object), fileID)
		if err != nil {
			return nil, err
		}
		return body, nil

	default:
		return nil, c.locationFault(object, "the object's provider is not recorded")
	}
}

// driveFileID returns the Drive file id recorded on the object.
func (c *contentSource) driveFileID(object model.LogicalObject) (string, error) {
	if c.drive == nil {
		return "", c.locationFault(object, "the drive provider is not configured on this deployment")
	}
	if object.ProviderFileID == nil || *object.ProviderFileID == "" {
		return "", c.locationFault(object, "no google drive file id is recorded")
	}
	return *object.ProviderFileID, nil
}

// scopeFor rebuilds the tenant scope an object belongs to.
//
// The repository layer is deliberately not involved: an object row already carries
// its tenant, and reaching back through a repository here would let a row from one
// tenant be read with another's credentials if the caller got it wrong.
func (c *contentSource) scopeFor(object model.LogicalObject) persistence.Scope {
	return persistence.Scope{TenantID: object.TenantID, UserID: object.UserID}
}

// Delete removes an object from its recorded provider only.
func (c *contentSource) Delete(ctx context.Context, object model.LogicalObject) error {
	switch object.Provider {
	case model.ProviderS3Compatible:
		if object.BackendID == nil {
			return c.locationFault(object, "no fallback backend is recorded")
		}
		client, ok := c.backends[*object.BackendID]
		if !ok {
			return c.locationFault(object, "the recorded fallback backend is not configured on this deployment")
		}
		return client.Delete(ctx, providerObject(object, client))

	case model.ProviderGoogleDrive:
		fileID, err := c.driveFileID(object)
		if err != nil {
			return err
		}
		return c.drive.DeleteFile(ctx, c.scopeFor(object), fileID)

	default:
		return c.locationFault(object, "the object's provider is not recorded")
	}
}

// providerObject builds the provider-level reference from the object row.
//
// The key and bucket come from the recorded row and the operator's backend
// configuration. Nothing about them is derived from a request, so a client cannot
// steer a read or a delete toward another tenant's prefix.
func providerObject(object model.LogicalObject, client *s3compat.Client) model.ProviderObject {
	out := model.ProviderObject{
		Provider:          model.ProviderS3Compatible,
		MIMEType:          object.MIMEType,
		SizeBytes:         object.VerifiedSize,
		Checksum:          object.VerifiedChecksum,
		ChecksumAlgorithm: object.ChecksumAlgorithm,
	}
	if object.ProviderObjectKey != nil {
		out.ObjectKey = *object.ProviderObjectKey
	}
	if object.BackendID != nil {
		out.Bucket = client.Bucket()
	}
	return out
}

// locationFault reports an inconsistent object row.
//
// It is an internal error because it means the catalog and the configuration
// disagree, which no user action can fix.
func (c *contentSource) locationFault(object model.LogicalObject, detail string) error {
	c.logger.Error("object location is unusable",
		"object_id", object.ID,
		"provider", string(object.Provider),
		"detail", detail,
	)
	return apperr.Internal("the object's storage location is inconsistent")
}

// providerUnavailable reports a recorded provider that cannot currently serve.
//
// The message is explicit that no second copy exists, because a client tempted to
// retry against "the other provider" needs to know that would not work.
func (c *contentSource) providerUnavailable(object model.LogicalObject, detail string) error {
	return &policy.ProviderError{
		Provider:   string(object.Provider),
		Operation:  "Content",
		Class:      policy.ClassUnavailable,
		SafeDetail: detail,
		Err:        errors.New(detail),
	}
}
