package httpapi

import (
	"context"

	"github.com/shovan04/storvia-project/internal/auth"
	"github.com/shovan04/storvia-project/internal/logger"
	"github.com/shovan04/storvia-project/internal/storage/model"
)

// loggerWithCorrelationID forwards to the logger package so the middleware tests
// can set a correlation ID without duplicating the context key.
func loggerWithCorrelationID(ctx context.Context, id string) context.Context {
	return logger.WithCorrelationID(ctx, id)
}

// requestIDFromContext reads the correlation ID back out.
func requestIDFromContext(ctx context.Context) string {
	return logger.CorrelationID(ctx)
}

// byteRangeType and parseRangeImpl alias the model types so the object-handler
// tests can stay in this package without importing model for two symbols.
type byteRangeType = model.ByteRange

func parseRangeImpl(header string) (*model.ByteRange, error) {
	return model.ParseByteRange(header)
}

// identityFromContext is a re-export used by the router tests.
func identityFromContext(ctx context.Context) (auth.Identity, bool) {
	return auth.IdentityFrom(ctx)
}
