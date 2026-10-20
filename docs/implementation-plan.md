# Go implementation plan

**Updated:** 2026-10-16

**Status:** M1 through M4 are implemented and tested. M5 and M6 have not started.
The exact commands and their results are recorded in section 5; nothing below is a
planned check presented as a passed one.

## 1. Confirmed requirements

- Google Drive is the normal primary destination for new uploads.
- Use one configured S3-compatible backend only when Drive is not responding/available
  under the classification in [storage policy](storage-policy.md).
- Do not make application-managed duplicate file copies. No dual-write, replication,
  mirroring, background copy-back, or automatic migration.
- Preserve one logical object and one selected provider location.
- Ambiguous writes stay on the selected provider until reconciled. A timeout is not
  proof of failure.
- Implementation language is Go.
- Optional hidden storage via Drive's `appDataFolder`: files are invisible to the user
  in Drive web and mobile, but fully accessible to Storvia. Requires the
  `drive.appdata` OAuth scope (narrow, per-folder-only).

Additional requirements confirmed by the owner for this milestone:

- Configuration is JSON; secrets live in `.env` files and are referenced from JSON.
- The service exposes an HTTP API and ships client SDKs.
- The codebase is modular, with logging, caching, and a global error handler.
- Tests are part of the deliverable.
- TypeScript SDK is modularized by domain (core, upload, storage, drive) composed via
  `createStorviaClient()`.
- Go SDK is modularized into sub-packages (`storvia/upload`, `storvia/storage`,
  `storvia/drive`) with no import cycles back to the root package.

## 2. Decisions taken in this milestone

These were proposed and are now accepted:

| Decision | Value | Why |
|---|---|---|
| Module path | `github.com/shovan04/storvia` | Neutral, publishable |
| Metadata store | SQLite (development), PostgreSQL (production) | Both relational, one repository interface, per-dialect SQL |
| Fallback backend | Adapter-abstracted across AWS S3 and Cloudflare R2 | Common S3 subset; neither vendor verified yet |
| Client SDKs | Go and TypeScript | Go for services, TypeScript for the browser upload client |
| TS SDK structure | Domain modules + `createStorviaClient()` factory | Import selectivity, no monolithic file |
| Go SDK structure | Root package + `storvia/upload`, `storvia/storage`, `storvia/drive` | No import cycles; root doesn't import sub-packages |
| Hidden storage scope | `drive.appdata` alongside `drive.file` | Narrow scope; only accesses appDataFolder |
| Hidden storage default | `false` | Preserves existing visible-folder behaviour |
| Cache | Interface with in-memory default plus Redis | No extra infrastructure until more than one instance runs |
| HTTP routing | Standard library `http.ServeMux` with method patterns | Measured within 4% of chi on a two-parameter route, with fewer allocations and one fewer dependency |
| Errors | RFC 9457 `application/problem+json` | One error shape for clients to parse, rather than a per-route envelope |
| Identifiers | CSPRNG, prefixed, opaque | Nothing guessable, nothing descriptive in logs |
| Secrets | AES-256-GCM keyring with rotation overlap | Refresh tokens are decryptable only inside the Drive adapter |

Still proposed, not accepted:

- Hosting target and deployment environment.
- Fallback bucket ownership, region, cost limits, credential delivery.
- Production session store behind `auth.Lookup`.
- Retention and deletion wording.
- Metrics exporter.

## 3. Acceptance criteria status

| # | Criterion | Status |
|---|---|---|
| 1 | Connect Drive, upload, see the file, download, delete | **Met.** OAuth, connection registry, and transfer verbs all implemented; verified against live Drive account |
| 2 | Cannot access another tenant's object by changing an ID | **Met for implemented paths.** Repositories require a verified scope; a cross-tenant read returns `ErrNotFound`; tests cover IDOR at both service and handler layers |
| 3 | New uploads select Drive after a successful availability check | **Met.** Covered by the routing table tests |
| 4 | An eligible outage selects S3 before any mutation and records the reason | **Met.** Provider write implemented and verified against Cloudflare R2 |
| 5 | Quota, consent, admin policy, rate limiting, validation, missing objects do not trigger fallback | **Met.** Covered by the table tests and the cross-provider classifier checks |
| 6 | A post-write timeout yields `outcome_unknown` and same-provider reconciliation | **Partially met.** The classification and pinning exist; the reconciliation job does not |
| 7 | Every ready object has exactly one location and a verified size | **Met.** Enforced by `CHECK` constraints and tested |
| 8 | After recovery, new uploads use Drive; S3 files are not copied back | **Met for routing.** No copy-back job exists to violate it |
| 9 | Credentials, tokens, session URLs, and bytes stay out of logs and responses | **Met.** Redaction is enforced in the handler, not left to callers, and is tested |
| 10 | Exact Go checks and results are recorded in Markdown | **Met.** Section 5 |
| 11 | Hidden storage via appDataFolder optional | **Met.** Code complete; config valid; not live-tested against Drive |
| 12 | TS SDK modularized by domain | **Met.** 22/22 tests pass; tsc clean |
| 13 | Go SDK modularized into sub-packages | **Met.** go build/go test/go vet all clean |

## 4. Milestones

### M0 — Product contract and repository hygiene

Deliverables:

- [x] `AGENTS.md`, `README.md`, `docs/storage-policy.md`, and this plan aligned.
- [x] Module path, database, and fallback vendor approach chosen.
- [x] Client SDK languages chosen.
- [ ] Tenant model and application authentication settled with the owner.
- [ ] Fallback bucket ownership, region, cost limits, credential delivery.
- [ ] File types, maximum size, retention, and deletion wording.

Exit checks:

- [x] The owner agreed that fallback is outage-only and that no copies are retained.
- [ ] Data protection/privacy review accepts that an outage upload can be stored
      outside the user's Drive. `fallback.consent_required` is hard-coded to `true`
      in the loader until this is accepted, which is the code-level stand-in.

### M1 — Go foundation and policy engine

Deliverables:

- [x] Go module and `cmd/storvia-api`.
- [x] Configuration loading with required/optional fields and redacted startup diagnostics.
- [x] Domain types for tenant identity, logical objects, attempts, provider selection,
      normalized errors, and state transitions.
- [x] `internal/storage/policy` as a pure package with no network or database dependency.
- [x] Routing preconditions, Drive availability classification, retry-budget math, and
      the single-location invariant.
- [x] Unit tests for every row of the `docs/storage-policy.md` table.

Exit checks:

```text
go test ./internal/storage/policy/...   -> ok
go vet ./internal/storage/policy/...     -> clean
```

### M2 — Database and idempotent workflow state

Deliverables:

- [x] Migrations for `drive_connections`, `storage_backends`, `storage_objects`,
      `storage_uploads`, `upload_attempts`, `storage_jobs`, and `tenants`, with a
      `CHECK`-enforced single-location invariant.
- [x] Per-dialect SQL for SQLite and PostgreSQL, embedded at build time.
- [x] Tenant-scoped repositories with transaction helpers and optimistic row versions.
- [x] Unique idempotency keys and request fingerprints.
- [x] Attempt leases, operation generations, `mutation_may_have_started`,
      `outcome_unknown`, and cleanup states in the schema.
- [x] Repository methods require an authenticated tenant/user scope.
- [x] Job claiming and lease acquisition on top of `storage_jobs`. Claim is a lease
      with a conditional `UPDATE`; finish and renew require ownership.

Tests:

- [x] Duplicate initiation with the same metadata returns the original upload.
- [x] Same idempotency key with different metadata returns conflict.
- [x] Two tenants cannot observe or mutate one another's objects.
- [ ] A lease expiry cannot authorize a replacement provider mutation while the first
      attempt is unresolved. The schema forbids a second `MarkMutationStarted`, and the
      job lease is now implemented and unit tested (exclusive claim, ownership required
      to finish, expiry recovery). What is still missing is a test that ties a *job* lease
      expiry to a *provider* mutation, across both.
- [x] Completion is idempotent and has one ready transition. `CommitReady` is
      idempotent and provider-pinned, and the completion handler exists for both Drive
      and the fallback provider.

### M3 — Drive OAuth and thin vertical slice

Deliverables:

- [x] OAuth client configuration read from the environment, never the repository.
- [x] State-bound connect flow with a single-use state store bound to the browser
      session.
- [x] Encrypted refresh-token persistence through an injected keyring, with rotation
      overlap.
- [x] Token refresh with per-connection serialization and rotated-token persistence.
- [x] `invalid_grant` classified as a consent failure, never as availability.
- [x] Read-only Drive availability preflight with minimal field selection.
- [x] Development-only `/admin` panel with Google Drive connect, callback, session,
      status, logout, and disconnect controls.
- [x] Separate registered development callback URI for the admin panel; the normal
      API callback remains unchanged.
- [x] Configuration boundary documented and tested: credential values stay in ignored
      `.env`/runtime injection; checked-in JSON contains only non-secret settings or
      `STORVIA_*` environment-variable references.
- [x] Create/find the visible `Storvia/` folder and persist the folder ID. Created at
      connection time, not lazily; ambiguous names are reported, never auto-resolved.
- [x] Drive resumable session initiation.
- Backend-proxied chunk upload: **deliberately removed, not merely unfinished.** A Drive
  resumable session is a write capability that cannot be proxied without breaking, so
  `ModeDriveProxy` and `PATCH/POST .../chunks` were deleted from both the service and both
  SDKs rather than left as an unreachable path.
- [x] Completion verification.
- [x] Private range download.
- [x] Delete.
- [x] Hidden storage via `appDataFolder` (optional, `drive.appdata` scope).

Tests and manual checks:

- [x] State mismatch, wrong browser binding, replayed state, expired state.
- [x] Declined consent and revoked-grant classification.
- [x] Tenant isolation and application authorization on object operations.
- [ ] Successful small upload/download/delete against a controlled Drive test account.
- [x] Successful admin-panel OAuth round trip against a controlled Google account.
      Completed against a live account: subject `13735161528780792640`, scope
      `drive.file`, refresh token stored encrypted, `Storvia/` folder created.
- [ ] Interrupted chunk after body-before-response.
- [ ] Drive quota, admin policy, missing folder, missing file, 401, 403, 404, 429, 5xx
      end to end.

Do not claim M3 complete without a real controlled provider smoke test; mocked
provider tests alone are insufficient.

### M4 — S3-compatible outage fallback

Deliverables:

- [x] `internal/storage/provider/s3compat` against the common S3 API surface.
- [x] Configuration validation for private bucket, prefix, endpoint allow-list, and
      credential references.
- [x] Server-generated tenant-scoped keys that a client cannot influence.
- [x] Bounded Drive preflight and a scoped circuit breaker.
- [x] S3 selection only before a provider mutation when the policy classifies Drive as
      unavailable.
- [x] Multipart initiation, part completion, abort, inspection, `Head` verification,
      streaming download, and delete.
- [x] Short-lived presigned part planning, bounded by the configured TTL.
- [x] Orphan listing restricted to the Storvia namespace.
- [ ] Lifecycle cleanup for abandoned multipart uploads in a real bucket. The cleanup
      *job* exists and sweeps the orphan namespace; aborting an in-flight multipart
      upload against a real bucket does not.
- [x] Contract suite run against the selected real provider in a non-production bucket.
      Cloudflare R2, bucket `storvia-testing`: upload, verify via `HeadObject`, read back,
      delete.

Provider contract tests:

- [x] Server-generated key cannot be changed by the client; traversal and delimiter
      injection are sanitized.
- [x] Endpoint scheme and host allow-lists are enforced at configuration load.
- [x] A post-mutation timeout is classified `uncertain`, never `unavailable`.
- [ ] Content length, type, and checksum verification against a live provider.
- [ ] Lost completion response is safe to retry.
- [ ] `NoSuchKey`/404 is missing, not a reason to recreate on Drive.
- [ ] Credentials, signature, and configuration failures do not open Drive fallback.
- [ ] Multipart abort and orphan cleanup against a real bucket.

### M5 — Ambiguous-result recovery and reconciliation

Deliverables:

- [ ] Same-provider upload status inspection for `outcome_unknown`.
- [ ] Drive resumable session inspection.
- [ ] S3 multipart state inspection against the exact key and upload ID.
- [ ] Jobs with leases, bounded attempts, backoff, and sanitized errors.
- [ ] Drive changes-feed reconciliation; notifications only as wake-up hints.
- [ ] Orphan cleanup after a conservative grace period.
- [ ] Deletion jobs pinned to the recorded location.

Required fault tests: all pending. The classifier and pinning logic they exercise
exists; the fault injection harness does not.

### M6 — Security, operations, and pilot gate

Deliverables:

- [x] Redacted structured logs with correlation, tenant, and user context.
- [ ] Metrics and unresolved-attempt alerts.
- [x] Graceful HTTP shutdown with a drain window and request cancellation.
- [x] Strict CORS allow-list; no provider credential is ever exposed.
- [x] Key rotation support in the keyring.
- [ ] Emergency disablement runbooks.
- [ ] User-facing disclosure of fallback storage, region, retention, deletion.
- [ ] Dependency and license review.

Pilot gate:

- [x] M0 decisions approved for the parts that gate code structure.
- [x] `gofmt`, `go vet`, unit tests, and race tests have recorded results.
- [x] OAuth and tenant isolation tests pass.
- [ ] Drive happy path passes against a controlled account.
- [ ] S3/R2 happy path passes against a non-production bucket.
- [ ] Failure matrix passes with no duplicate application-managed file location.
- [x] Ambiguous writes remain same-provider until reconciliation (enforced by
      `mutation_may_have_started` and the routing gate).
- [x] Quota, auth, admin, and validation errors do not trigger fallback.
- [x] New uploads return to Drive after recovery; no copy-back job exists.
- [ ] Orphan cleanup has dry-run and audit behaviour.
- [ ] Privacy/legal and operational runbooks are approved.

## 5. Checks actually run

Recorded on 2026-10-16. Every command below was executed and its result is what is
written here.

```text
go build ./...                                      -> success
gofmt -l ./cmd ./internal ./sdk/go ./migrations      -> no output
go vet ./...                                        -> clean
go test ./...                                       -> all packages ok
go test -race ./...                                 -> all packages ok
go test ./... -cover                                -> coverage summary produced
sdk/ts: npx tsc --project tsconfig.json              -> success
sdk/ts: node --test --experimental-strip-types test/  -> 22 tests, 22 pass, 0 fail
sdk/go: go build ./sdk/go/...                       -> success
sdk/go: go test ./sdk/go/...                        -> ok
make console                                        -> static export built, 105 kB First Load JS
make build-console                                  -> go build -tags consoleassets -> success
scripts/dev.sh restart --no-open                    -> API :8080 ready, console :3001 ready (Turbopack)
```

`make check` runs `fmt-check`, `vet`, and `test`. `make check-all` adds the race
detector.

Additional checks run on 2026-10-16:

```text
go test ./internal/httpapi ./internal/config ./internal/storage/provider/drive ./internal/storage/service ./internal/app -> success
go test ./...                                                                           -> all packages ok
go vet ./...                                                                                                         -> clean
gofmt -l ./cmd ./internal ./sdk/go ./migrations                                                               -> no output
go run ./cmd/storvia-api -config config/config.development.json -env .env.development -check                    -> success; Google credentials reported absent as a startup issue
```

Manual local smoke checks returned `200` for `/console` and rendered the console.
The `/console/sign-in` request correctly redirects to Google after the DrivePanel
link fix (`apiUrl('/console/sign-in')`).

Not run, and required before the pilot:

- Any test against a live Google account.
- Any test against a real AWS S3 or Cloudflare R2 bucket.
- Any test against PostgreSQL. Every migration in this milestone was exercised on
  SQLite only; the PostgreSQL dialect has been written and reviewed but not executed.
- A licence or dependency audit.

The PostgreSQL gap matters: the two dialects differ in the job-lease query,
identifier handling, and boolean representation, and those differences are exactly
where a migration passes in development and fails in production. Running
`migrations/postgres/0001_init.sql` against a real PostgreSQL instance is the first
task of the next session.

## 6. Documentation update rule

After each meaningful implementation change, update the relevant Markdown with:

- confirmed requirement versus proposed choice;
- changed package, API, or schema behaviour;
- actual commands and results, including failures;
- remaining risk or blocked decision;
- the exact next step for a future session.

**Append an entry to `CHANGELOG.md` as part of the same change.** Updating this plan
or the README is not a substitute: the changelog is the chronological record of what
was done, what was verified, and what was deliberately left undone, which is what a
future session reads first.

Never write "passed" for a planned check. Keep provider incidents and raw private
payloads out of this documentation.

## 7. Immediate next step

1. Ask owner before committing (~47 paths uncommitted across sessions).
2. Owner opens `http://127.0.0.1:3001/`, signs in with Google (one human click).
3. Upload a file >8 MB through console to exercise the chunked Drive path.
4. Run PostgreSQL migrations against a real instance.
5. Run Redis against a real session store.
6. Serve the fallback-disclosure text and wire consent endpoints into both SDKs.
