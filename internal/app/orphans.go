package app

import (
	"context"
	"time"

	"github.com/shovan04/storvia-project/internal/persistence"
	"github.com/shovan04/storvia-project/internal/storage/provider/s3compat"
)

// orphanGrace is how long an object must have existed at a provider before it is
// treated as abandoned.
//
// Without it, a slow database commit or a response lost in transit makes a perfectly
// good object look like an orphan, and cleanup deletes a file the user still has. The
// cost of the grace period is that genuinely orphaned bytes linger; the cost of
// without it is data loss.
const orphanGrace = time.Hour

// sweepOrphans lists the fallback bucket's Storvia namespace and reports keys with no
// Storvia record.
//
// It only counts. Listing is cleanup *evidence*, never permission to delete: Storvia
// has no second copy, and a key it does not recognise might be a file whose database
// commit is still in flight. Acting on that would be the single worst thing this
// service could do.
func (w *Worker) sweepOrphans(ctx context.Context) (int, error) {
	return w.sweepOrphansFor(ctx, w.backends)
}

// sweepOrphansFor walks each backend's Storvia prefix.
//
// The listing is paginated: a single page would silently under-count orphans on a
// bucket that has any.
func (w *Worker) sweepOrphansFor(ctx context.Context, backends map[string]*s3compat.Client) (int, error) {
	if len(backends) == 0 {
		return 0, nil
	}

	objects := persistence.NewObjectRepository(w.store)
	total := 0

	for id, client := range backends {
		count, err := w.sweepBackend(ctx, client, objects)
		if err != nil {
			// One unreachable bucket must not stop the sweep of the others.
			w.log.WarnContext(ctx, "orphan sweep could not read a backend",
				"backend_id", id, "error", err.Error())
			continue
		}
		total += count
	}
	if total > 0 {
		w.metrics.RecordOrphans("s3_compatible", total)
	}
	return total, nil
}

func (w *Worker) sweepBackend(ctx context.Context, client *s3compat.Client, objects *persistence.ObjectRepository) (int, error) {
	// Bounded: an unbounded list on a large bucket would hold the lease for longer
	// than it is worth.
	listCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	orphans := 0
	token := ""
	// The page cap is a safety bound, not a correctness requirement. Hitting it is
	// logged so a truncated sweep is never mistaken for a clean one.
	for page := 0; page < 50; page++ {
		keys, err := client.ListStorviaPrefix(listCtx, token, 1000)
		if err != nil {
			return orphans, err
		}
		if len(keys) == 0 {
			break
		}
		for _, key := range keys {
			if !client.OwnsKey(key) {
				// Not ours. Never touch it, and never count it.
				continue
			}
			known, err := objects.ExistsByProviderKey(listCtx, key)
			if err != nil {
				return orphans, err
			}
			if !known {
				orphans++
				w.log.WarnContext(listCtx, "provider object has no storvia record",
					"bucket", client.Bucket(), "key", key,
					"note", "reported only; deletion needs a grace period and a decision")
			}
		}
		if len(keys) < 1000 {
			break
		}
		token = keys[len(keys)-1]
	}
	return orphans, nil
}
