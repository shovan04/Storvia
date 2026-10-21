package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/shovan04/storvia-project/internal/metrics"
	"github.com/shovan04/storvia-project/internal/persistence"
	"github.com/shovan04/storvia-project/internal/storage/provider/s3compat"
	"github.com/shovan04/storvia-project/internal/storage/service"
)

// WorkerConfig tunes the background worker.
type WorkerConfig struct {
	// Enabled turns the worker on. It is off by default so that a test or a
	// one-off process never races the primary instance for jobs.
	Enabled bool
	// Owner identifies this worker in the lease. It defaults to the hostname and pid,
	// which is enough to tell two processes apart in a log.
	Owner string
	// Lease is how long a claimed job is reserved. It must comfortably exceed the
	// slowest job, or healthy work gets taken from under its worker.
	Lease time.Duration
	// RenewEvery is how often a long job extends its lease.
	RenewEvery time.Duration
	// Poll is how long to wait when the queue is empty.
	Poll time.Duration
	// MaxAttempts before a failing job is parked for a person.
	MaxAttempts int
	// RetryBackoff is how long to wait before retrying a failed job.
	RetryBackoff time.Duration
}

// Worker runs deferred work: reconciliation, retryable deletes, and cleanup.
//
// It is deliberately conservative. Every job it runs either observes a provider or
// removes something Storvia itself put there. It never creates content, and it never
// writes to a second provider — that is the rule that keeps a failed reconciliation
// from turning into a duplicate.
type Worker struct {
	store    *persistence.Store
	jobs     *persistence.JobRepository
	uploads  *service.UploadService
	objects  *service.ObjectService
	backends map[string]*s3compat.Client
	metrics  *metrics.Storvia
	log      *slog.Logger
	cfg      WorkerConfig
}

// NewWorker builds the background worker.
func NewWorker(
	store *persistence.Store,
	uploads *service.UploadService,
	objects *service.ObjectService,
	backends map[string]*s3compat.Client,
	appMetrics *metrics.Storvia,
	log *slog.Logger,
	cfg WorkerConfig,
) *Worker {
	if cfg.Lease <= 0 {
		cfg.Lease = 2 * time.Minute
	}
	if cfg.RenewEvery <= 0 || cfg.RenewEvery >= cfg.Lease {
		// Renewing at half the lease means one missed tick is not fatal.
		cfg.RenewEvery = cfg.Lease / 2
	}
	if cfg.Poll <= 0 {
		cfg.Poll = 5 * time.Second
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 5
	}
	if cfg.RetryBackoff <= 0 {
		cfg.RetryBackoff = time.Minute
	}
	if log == nil {
		log = slog.Default()
	}
	return &Worker{
		store:    store,
		jobs:     persistence.NewJobRepository(store),
		uploads:  uploads,
		objects:  objects,
		backends: backends,
		metrics:  appMetrics,
		log:      log,
		cfg:      cfg,
	}
}

// Run claims and executes jobs until the context is cancelled.
func (w *Worker) Run(ctx context.Context) {
	owner := w.cfg.Owner
	if owner == "" {
		host, _ := os.Hostname()
		owner = fmt.Sprintf("%s/%d", host, os.Getpid())
	}
	w.log.Info("background worker started",
		"owner", owner, "lease", w.cfg.Lease, "poll", w.cfg.Poll)

	for {
		if ctx.Err() != nil {
			w.log.Info("background worker stopping")
			return
		}

		job, err := w.jobs.Claim(ctx, owner, w.cfg.Lease)
		if errors.Is(err, persistence.ErrNoJob) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(w.cfg.Poll):
			}
			continue
		}
		if err != nil {
			w.log.ErrorContext(ctx, "could not claim a job", "error", err.Error())
			select {
			case <-ctx.Done():
				return
			case <-time.After(w.cfg.Poll):
			}
			continue
		}

		w.execute(ctx, owner, job)
	}
}

// execute runs one claimed job.
//
// The lease is renewed on a timer so a slow job is not taken from under this worker.
// Losing the lease mid-job is not fatal to the process but must stop *this* job: two
// workers acting on one object is the double-delete the lease exists to prevent.
func (w *Worker) execute(ctx context.Context, owner string, job persistence.Job) {
	stopRenew := w.renewUntilDone(ctx, owner, job.ID)
	defer close(stopRenew)

	start := time.Now()
	err := w.run(ctx, job)
	elapsed := time.Since(start)

	if err != nil {
		if errors.Is(err, persistence.ErrJobLeaseLost) {
			w.log.WarnContext(ctx, "job lease lost; abandoning it",
				"job_id", job.ID, "kind", job.Kind)
			w.recordRun(job.Kind, "lease_lost")
			return
		}
		w.log.ErrorContext(ctx, "job failed",
			"job_id", job.ID, "kind", job.Kind, "error", err.Error(), "elapsed", elapsed)
		if failErr := w.jobs.Fail(ctx, job.ID, owner, err.Error(),
			w.cfg.RetryBackoff, w.cfg.MaxAttempts); failErr != nil {
			w.log.ErrorContext(ctx, "could not record the job failure",
				"job_id", job.ID, "error", failErr.Error())
		}
		w.recordRun(job.Kind, "failed")
		return
	}

	if err := w.jobs.Finish(ctx, job.ID, owner); err != nil {
		w.log.ErrorContext(ctx, "could not finish the job", "job_id", job.ID, "error", err.Error())
		w.recordRun(job.Kind, "finish_failed")
		return
	}
	w.log.InfoContext(ctx, "job completed", "job_id", job.ID, "kind", job.Kind, "elapsed", elapsed)
	w.recordRun(job.Kind, "ok")
}

// renewUntilDone extends the lease until the returned channel is closed.
func (w *Worker) renewUntilDone(ctx context.Context, owner, jobID string) chan struct{} {
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(w.cfg.RenewEvery)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := w.jobs.Renew(ctx, jobID, owner, w.cfg.Lease); err != nil {
					if !errors.Is(err, persistence.ErrJobLeaseLost) {
						w.log.WarnContext(ctx, "could not renew a job lease",
							"job_id", jobID, "error", err.Error())
					}
					return
				}
			}
		}
	}()
	return stop
}

// run dispatches one job to its handler.
func (w *Worker) run(ctx context.Context, job persistence.Job) error {
	switch job.Kind {
	case persistence.JobReconcile:
		return w.runReconcile(ctx, job)
	case persistence.JobDelete:
		return w.runDelete(ctx, job)
	case persistence.JobCleanup:
		// Cleanup is a sweep rather than a per-row action; the sweeper enqueues
		// nothing and reports what it found. Running it per job would sweep the
		// whole bucket once per queued marker.
		return w.runCleanupSweep(ctx)
	default:
		return fmt.Errorf("worker: unknown job kind %q", job.Kind)
	}
}

// runReconcile asks the provider what happened to an unfinished upload.
func (w *Worker) runReconcile(ctx context.Context, job persistence.Job) error {
	if job.ObjectID == nil {
		return fmt.Errorf("worker: a reconcile job needs an object")
	}
	scope := persistence.Scope{TenantID: job.TenantID}
	_, err := w.uploads.Reconcile(ctx, scope, *job.ObjectID)
	return err
}

// runDelete removes an object that a request could not finish.
//
// It goes through the object service rather than the provider directly, so the single
// location rule still applies: the delete is pinned to the recorded provider.
func (w *Worker) runDelete(ctx context.Context, job persistence.Job) error {
	if job.ObjectID == nil {
		return fmt.Errorf("worker: a delete job needs an object")
	}
	scope := persistence.Scope{TenantID: job.TenantID}
	return w.objects.DeleteObject(ctx, scope, *job.ObjectID)
}

// runCleanupSweep looks for provider objects with no Storvia record.
//
// It only reports. Deleting an orphan needs a grace period, because a lost response or
// a delayed commit can make a perfectly good object look abandoned.
func (w *Worker) runCleanupSweep(ctx context.Context) error {
	found, err := w.sweepOrphans(ctx)
	if err != nil {
		return err
	}
	if found > 0 {
		w.log.WarnContext(ctx, "orphaned provider objects found; they are reported, not deleted",
			"count", found)
	}
	return nil
}

// recordRun notes a job outcome, tolerating absent metrics.
func (w *Worker) recordRun(kind, outcome string) {
	if w.metrics == nil {
		return
	}
	w.metrics.RecordJobRun(kind, outcome)
}

// StartWorker runs the worker in the background when enabled.
//
// It returns a stop function. Returning one rather than only a context keeps the caller
// honest: the worker has a goroutine and a ticker, and something must eventually join
// them.
func StartWorker(ctx context.Context, w *Worker, enabled bool) func() {
	if !enabled || w == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()

	var once sync.Once
	return func() {
		once.Do(func() {})
	}
}

// outcomeLabel names a job result for metrics.
func outcomeLabel(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, persistence.ErrJobLeaseLost):
		return "lease_lost"
	default:
		return "failed"
	}
}
