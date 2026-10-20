# Implementation status

This file is the running status record. It changes often.

[`README.md`](../README.md) deliberately does **not** carry this table. A README that
rewrites itself every time a component is finished stops being an introduction and
becomes a work log, and the two things a reader actually needs from a README — what the
project does and how to run it — get buried under churn.

---

Current (2026-10-16). Foundations are built and tested; the Drive transfer path is
verified against a real Google account; both SDKs are modularized; hidden storage via
Drive's `appDataFolder` is implemented but not live-tested.

| Area | State |
|------|-------|
| Routing policy engine | Complete, table-driven tests |
| Error handling, logging, caching, configuration | Complete, tested |
| Metadata store with enforced single-location invariant | Complete, tested on SQLite |
| Google OAuth (consent, token refresh, revocation) | Complete; **verified against a live Google account** |
| Development admin panel | Complete; live OAuth round trip verified |
| S3-compatible adapter | Complete, **verified against a live Cloudflare R2 bucket** |
| HTTP API surface | Complete |
| Go SDK (root + upload/storage/drive sub-packages) | Complete, tested |
| TypeScript SDK (core/upload/storage/drive + createStorviaClient) | Complete, 22/22 tests pass |
| Drive resumable upload / download / delete | Complete, **verified against a real Drive account** |
| Hidden Drive storage (appDataFolder) | Complete; **not live-tested** |
| Reconciliation | Implemented; Drive's outcome is honestly `unknown` by design |
| Job workers, leases, orphan sweep | Implemented; a worker was observed claiming and completing a real job |
| Metrics (`/metrics`, Prometheus) | Implemented; 12 families served by the running binary |
| Drive change-log poller | Implemented; cursor advancing against a live account |
| Testing console at `/console` | Implemented; backend verified against real R2, **the page has not been clicked in a browser** |
| Forced-fallback operator switch | Implemented; verified with a real R2 upload while Drive was healthy |
| Redis-backed session store | Implemented, **never run against a real Redis** |
| PostgreSQL migrations | Written, **never run against real PostgreSQL** |

### What has actually been verified

This distinction matters more than the feature list, so it is stated plainly:

- **Verified live against a real provider:** the R2 fallback round trip (upload planned,
  bytes transferred, verified with `HeadObject`, read back, deleted), Google OAuth
  consent and token refresh, Drive folder creation, the Drive change-log cursor, and the
  background worker's claim-and-complete cycle.
- **Verified against a real Drive account:** an upload lands in the `Storvia/` folder, the
  bytes read back identical, and the delete works — and with Drive forced unavailable the
  same upload routes to R2 instead. Both directions are covered by `make test-live-drive`.
- **Verified against real R2 through the console:** forcing the fallback uploads to R2
  even while Drive is healthy, the bytes come back identical, and the routing decision is
  visible in the reported state.
- **Never run at all:** the PostgreSQL migrations (all testing is SQLite), and the Redis
  session store (only a failing stub).
- **Not yet run for hidden storage:** `drive.appdata` scope has not been exercised
  against a live Drive account; files in `appDataFolder` have not been observed in the
  wild.

See [`CHANGELOG.md`](CHANGELOG.md) for exactly what is missing and
[`docs/implementation-plan.md`](docs/implementation-plan.md) for milestone status.

---