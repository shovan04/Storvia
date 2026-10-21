package app

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/shovan04/storvia-project/internal/storage/policy"
	"github.com/shovan04/storvia-project/internal/storage/provider/drive"
)

// ChangesPoller advances Drive's change log for every connected account.
//
// It is a poller rather than a webhook subscriber on purpose. Push notifications are
// only a hint that something changed: they can be delivered more than once, missed
// while a deployment is down, and arrive for accounts Storvia no longer cares about.
// The change log is the source of truth; this walks it from a stored cursor.
type ChangesPoller struct {
	drive *drive.Client
	log   *slog.Logger

	// Interval is how long to wait between passes.
	Interval time.Duration
	// Batch is how many changes to read per page.
	Batch int
}

// NewChangesPoller builds the poller.
func NewChangesPoller(client *drive.Client, log *slog.Logger) *ChangesPoller {
	if log == nil {
		log = slog.Default()
	}
	return &ChangesPoller{
		drive:    client,
		log:      log,
		Interval: 5 * time.Minute,
		Batch:    500,
	}
}

// PollOnce advances the change log for one connection and returns the cursor it
// should store.
//
// The cursor is advanced *after* the page is processed, never before. Advancing first
// and then failing would skip changes permanently; advancing second re-reads a page on
// a crash, and every change here is idempotent, so re-reading is the safe direction.
func (p *ChangesPoller) PollOnce(ctx context.Context, connectionID string) (string, error) {
	cursor, err := p.drive.ChangeCursor(ctx, connectionID)
	if err != nil {
		return cursor, err
	}

	page, err := p.drive.ListChanges(ctx, connectionID, cursor, p.Batch)
	if err != nil {
		return cursor, err
	}

	if len(page.Changes) > 0 {
		p.log.InfoContext(ctx, "drive changes observed",
			"connection_id", connectionID,
			"changes", len(page.Changes),
			"removed", countRemoved(page.Changes))
	}

	// An exhausted log returns no next cursor, which means "caught up". The newest
	// start token is where the next poll begins, so it is what gets stored.
	next := page.NextCursor
	if next == "" {
		next = page.NewestCursor
	}
	if next == "" {
		// Nothing to record: an account with no changes at all reports neither.
		return cursor, nil
	}
	if err := p.drive.AdvanceChangesCursor(ctx, connectionID, next); err != nil {
		return cursor, err
	}
	return next, nil
}

// Run polls until the context is cancelled.
func (p *ChangesPoller) Run(ctx context.Context, connections func(context.Context) ([]string, error)) {
	if p.drive == nil {
		return
	}
	interval := p.Interval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	p.log.Info("drive changes poller started", "interval", interval)

	for {
		if ctx.Err() != nil {
			p.log.Info("drive changes poller stopping")
			return
		}
		p.pollAll(ctx, connections)

		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func (p *ChangesPoller) pollAll(ctx context.Context, connections func(context.Context) ([]string, error)) {
	if connections == nil {
		return
	}
	ids, err := connections(ctx)
	if err != nil {
		p.log.WarnContext(ctx, "could not list drive connections for polling", "error", err.Error())
		return
	}
	for _, id := range ids {
		if _, err := p.PollOnce(ctx, id); err != nil {
			var providerErr *policy.ProviderError
			if errors.As(err, &providerErr) {
				// One account failing must not stop the others. A revoked grant on
				// one tenant says nothing about the rest.
				// The provider's own reason is included because "google returned
				// 400" is not actionable, and this runs unattended.
				p.log.WarnContext(ctx, "drive change poll deferred",
					"connection_id", id,
					"class", string(providerErr.Class),
					"status", providerErr.StatusCode,
					"reason", providerErr.SafeDetail,
					"provider_reason", providerErr.Reason)
				continue
			}
			// One account failing must not stop the others; a revoked grant on one
			// tenant says nothing about the rest.
			p.log.ErrorContext(ctx, "drive change poll failed",
				"connection_id", id, "error", err.Error())
		}
	}
}

func countRemoved(changes []drive.Change) int {
	n := 0
	for _, change := range changes {
		if change.Removed {
			n++
		}
	}
	return n
}

// StartChangesPoller runs the poller in the background and returns a stop function.
func StartChangesPoller(ctx context.Context, poller *ChangesPoller, enabled bool, connections func(context.Context) ([]string, error)) func() {
	if !enabled || poller == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		poller.Run(ctx, connections)
	}()

	var once sync.Once
	return func() {
		once.Do(func() {})
		<-done
	}
}
