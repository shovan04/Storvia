/**
 * Upload domain for the Storvia TypeScript SDK.
 *
 * Handles upload creation, Drive resumable sessions, presigned parts, and the
 * chunked upload helper. Composed into {@link StorviaClient}.
 */

/** The transfer mechanism the server selected. The client never chooses a provider. */
export type UploadMode = 'drive_resumable_session' | 's3_presigned_parts';

/**
 * A Drive resumable upload session.
 *
 * `url` is a write capability for exactly one file: anyone holding it can write to
 * that file until the session expires. Treat it as a secret. Storvia hands it out once
 * and never stores it, so it cannot be recovered if lost.
 */
export interface DriveSession {
  url: string;
  chunk_size: number;
  expires_at: string;
}

/** What the server found when it asked the provider about an unfinished upload. */
export interface ReconcileResult {
  upload_id: string;
  object_id: string;
  provider: string;
  state: string;
  /**
   * - `already_ready` the upload finished long ago
   * - `completed` the provider has the bytes; Storvia recorded that
   * - `nothing_stored` the provider is certain nothing landed; safe to retry
   * - `size_mismatch` something is there but not what was declared
   * - `unknown` nobody can tell, which is NOT the same as `nothing_stored`
   *
   * Only `nothing_stored` means it is safe to start the upload again.
   */
  outcome: string;
  reason?: string;
  location?: string;
}

/** The reply from the complete endpoint. */
interface CompleteResponse {
  object: import('./storage.ts').StorviaObject;
  already_complete: boolean;
}

/** A new upload request. */
export interface CreateUploadRequest {
  /** Display name. It is never used as a storage key. */
  name: string;
  /** Untrusted metadata used for routing and download headers. */
  mime_type: string;
  size_bytes: number;
  category: string;
}

/** The server's routing decision plus transfer instructions. */
export interface CreateUploadResult {
  upload_id: string;
  object_id: string;
  mode: UploadMode;
  /** Set in Drive mode. Its URL is a write capability for this one file. */
  session?: DriveSession;
  provider: string;
  state: string;
  /** True when this answers an idempotent repeat rather than a new provider write. */
  replayed: boolean;
  /** Non-empty only when Drive was unavailable and the fallback was used. */
  fallback_reason?: string;
  chunk_size: number;
  uploaded_bytes: number;
  expires_at?: string;
  parts?: PresignedPart[];
}

/** One short-lived upload capability. Treat the URL as a bearer token. */
export interface PresignedPart {
  part_number: number;
  url: string;
  headers?: Record<string, string>;
  expires_at: string;
}

/** The state of an upload. */
export interface UploadView {
  upload_id: string;
  object_id: string;
  state: string;
  provider: string;
  fallback_reason?: string;
  uploaded_bytes: number;
  expected_bytes: number;
  attempt_state: string;
  /** When true the upload is pinned to its provider and must not be restarted. */
  mutation_may_have_started: boolean;
  created_at: string;
  updated_at: string;
}

/** Options for a single request. */
export interface RequestOptions {
  /** Abort the request. */
  signal?: AbortSignal;
  /** Stable across retries of the same logical request. */
  idempotencyKey?: string;
}

/** The upload domain client. Composed into StorviaClient. */
export class UploadClient {
  public request: StorviaClient['request'];
  private readonly fetchImpl: typeof fetch;

  constructor(client: StorviaClient) {
    this.request = client.request.bind(client);
    this.fetchImpl = client.fetchImpl;
  }

  /**
   * Starts a new upload.
   *
   * `idempotencyKey` must be stable across retries of the same logical request. A
   * fresh key per attempt defeats the protection and can create duplicate files.
   */
  async createUpload(
    request: CreateUploadRequest,
    options?: RequestOptions,
  ): Promise<CreateUploadResult> {
    if (!options?.idempotencyKey) {
      throw new Error(
        'storvia: an idempotencyKey is required; a fresh key per attempt can create duplicate files',
      );
    }
    return this.request<CreateUploadResult>('POST', '/v1/storage/uploads', {
      body: request,
      options,
    });
  }

  /** Reads an upload's state. */
  getUpload(uploadId: string, options?: RequestOptions): Promise<UploadView> {
    return this.request<UploadView>('GET', `/v1/storage/uploads/${encodeURIComponent(uploadId)}`, {
      options,
    });
  }

  /**
   * Abandons an upload that has not begun writing bytes.
   *
   * The server refuses this once the upload may already have written data. That
   * refusal is correct and must not be worked around by starting a new upload.
   */
  cancelUpload(uploadId: string, options?: RequestOptions): Promise<void> {
    return this.request<void>(
      'DELETE',
      `/v1/storage/uploads/${encodeURIComponent(uploadId)}`,
      { options },
    );
  }

  /**
   * Finalizes an upload.
   *
   * For the Drive mode, `driveFileId` is the id Google returned in the final chunk
   * response. Storvia treats it as untrusted input and re-reads the file from Drive,
   * adopting it only if it sits in the Storvia folder, is not trashed, and has the
   * declared size.
   *
   * Idempotent: repeating it returns the existing object.
   */
  async completeUpload(
    uploadId: string,
    driveFileId?: string,
    options?: RequestOptions,
  ): Promise<import('./storage.ts').StorviaObject> {
    const response = await this.request<CompleteResponse>(
      'POST',
      `/v1/storage/uploads/${encodeURIComponent(uploadId)}/complete`,
      { body: driveFileId ? { drive_file_id: driveFileId } : undefined, options },
    );
    return response.object;
  }

  /**
   * Asks the server what actually happened to an unfinished upload.
   *
   * It reports; it never retries and never re-uploads. Read the outcome before
   * starting over: an upload that is merely unknown may well have succeeded.
   */
  async reconcileUpload(
    uploadId: string,
    options?: RequestOptions,
  ): Promise<ReconcileResult> {
    return this.request<ReconcileResult>(
      'POST',
      `/v1/storage/uploads/${encodeURIComponent(uploadId)}/reconcile`,
      { options },
    );
  }

  /**
   * Uploads a file through the Drive proxy path, chunk by chunk.
   *
   * The chunk boundaries come from the server. On an unknown outcome the helper
   * stops and reports it rather than retrying, because retrying an upload whose
   * bytes may already be at the provider is how duplicates are created.
   */
  async upload(
    request: CreateUploadRequest,
    body: Blob,
    options?: RequestOptions,
  ): Promise<{ result: CreateUploadResult; object: import('./storage.ts').StorviaObject }> {
    const result = await this.createUpload(request, options);

    if (result.mode === 's3_presigned_parts') {
      return { result, object: await this.completeUpload(result.upload_id, undefined, options) };
    }

    const session = result.session;
    if (!session || !session.url) {
      throw new StorviaError({
        kind: 'unavailable',
        status: 502,
        title: 'No Drive session',
        detail: 'The server did not return a Drive resumable session for this upload.',
      });
    }

    const driveFileId = await this.uploadToDriveSession(session, result.chunk_size, body);

    return {
      result,
      object: await this.completeUpload(result.upload_id, driveFileId, options),
    };
  }

  /**
   * Streams a file into a Drive resumable session and returns the resulting file id.
   *
   * Every chunk except the last is a multiple of 256 KiB, because Drive rejects
   * anything else, and each chunk declares its own byte range. Without that range a
   * retry cannot be distinguished from the original and the file ends up with a
   * hole in it.
   */
  private async uploadToDriveSession(
    session: DriveSession,
    chunkSize: number,
    body: Blob,
  ): Promise<string> {
    const size = Math.max(chunkSize || session.chunk_size || 256 * 1024, 1);
    let offset = 0;
    let fileId: string | undefined;

    while (offset < body.size) {
      const end = Math.min(offset + size, body.size);
      const slice = body.slice(offset, end);

      const response = await this.fetchImpl(session.url, {
        method: 'PUT',
        headers: {
          'Content-Type': 'application/octet-stream',
          'Content-Range': `bytes ${offset}-${end - 1}/${body.size}`,
        },
        body: slice,
      });

      if (response.status === 200 || response.status === 201) {
        const created = (await response.json()) as { id?: string };
        fileId = created.id;
      } else if (response.status !== 308) {
        throw new StorviaError({
          kind: 'unavailable',
          status: 502,
          title: 'Drive rejected a chunk',
          detail:
            `Google Drive rejected the byte range ${offset}-${end - 1}/${body.size} ` +
            `with status ${response.status}.`,
        });
      }
      offset = end;
    }

    if (!fileId) {
      throw new StorviaError({
        kind: 'unavailable',
        status: 502,
        title: 'No file id',
        detail: 'Google Drive did not return a file id, so the upload cannot be completed.',
      });
    }
    return fileId;
  }
}

import { StorviaError } from './core.ts';
import type { StorviaClient } from './core.ts';
