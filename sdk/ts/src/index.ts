/**
 * Public entry point for the Storvia TypeScript SDK.
 *
 * The SDK is modular: each domain (upload, storage, drive) is a separate module
 * composed into {@link StorviaClient} via {@link createStorviaClient}.
 *
 * Use {@link createStorviaClient} to build a fully wired client:
 *
 * ```ts
 * import { createStorviaClient } from '@storvia/sdk';
 * const client = createStorviaClient({ baseUrl: 'https://api.storvia.example' });
 * const { result, object } = await client.uploadClient.upload(
 *   { name: 'report.pdf', mime_type: 'application/pdf', size_bytes: 2048, category: 'report' },
 *   fileBlob,
 * );
 * ```
 *
 * Individual domain clients are also exported for advanced use cases where only
 * one domain is needed.
 */

// Base client, errors, and shared types.
export {
  StorviaClient,
  StorviaError,
  TransportError,
  type ErrorKind,
  type Problem,
  type RequestOptions,
  type StorviaClientOptions,
} from './core.ts';

// Upload domain.
export {
  UploadClient,
  type UploadMode,
  type DriveSession,
  type CreateUploadRequest,
  type CreateUploadResult,
  type PresignedPart,
  type UploadView,
  type ReconcileResult,
} from './upload.ts';

// Storage domain.
export {
  StorageClient,
  type StorviaObject,
  type StorageStatus,
} from './storage.ts';

// Drive domain.
export {
  DriveClient,
  type DriveConnection,
} from './drive.ts';

import { StorviaClient } from './core.ts';
import { UploadClient } from './upload.ts';
import { StorageClient } from './storage.ts';
import { DriveClient } from './drive.ts';

/**
 * Creates a fully wired StorviaClient with all domain clients composed.
 *
 * This is the preferred way to construct a client. Using `new StorviaClient()`
 * directly leaves the domain clients unset; domain methods will throw until
 * they are wired.
 */
export function createStorviaClient(options: import('./core.ts').StorviaClientOptions): import('./core.ts').StorviaClient {
  const client = new StorviaClient(options);
  client.upload = new UploadClient(client);
  client.storage = new StorageClient(client);
  client.drive = new DriveClient(client);
  return client;
}
