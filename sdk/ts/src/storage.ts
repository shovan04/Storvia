/**
 * Storage domain for the Storvia TypeScript SDK.
 *
 * Handles object metadata, downloads, and the storage status endpoint.
 * Composed into {@link StorviaClient}.
 */

/** A stored file. Provider identifiers are deliberately absent. */
export interface StorviaObject {
  object_id: string;
  name: string;
  mime_type: string;
  size_bytes: number;
  status: string;
  provider: string;
  category: string;
  fallback_reason?: string;
  verified_at?: string;
  created_at: string;
}

/** Whether Storvia can accept an upload, and why not when it cannot. */
export interface StorageStatus {
  primary_provider: string;
  drive_connected: boolean;
  fallback_enabled: boolean;
  fallback_vendor?: string;
  fallback_ready: boolean;
  fallback_requires_consent: boolean;
  /** The single answer needed before offering an upload control. */
  accepts_uploads: boolean;
  blocked_reason?: string;
  /** True when outage uploads may be stored outside the user's Drive. */
  fallback_stores_outside_drive: boolean;
}

/** Options for a single request. */
export interface RequestOptions {
  /** Abort the request. */
  signal?: AbortSignal;
  /** Stable across retries of the same logical request. */
  idempotencyKey?: string;
}

/** RFC 9457 problem details, as returned for every failure. */
interface Problem {
  type: string;
  title: string;
  status: number;
  detail: string;
  instance?: string;
  request_id?: string;
  fields?: Record<string, string>;
}

/** The storage domain client. Composed into StorviaClient. */
export class StorageClient {
  public request: StorviaClient['request'];
  private readonly fetchImpl: typeof fetch;
  private readonly baseUrl: string;

  constructor(client: StorviaClient) {
    this.request = client.request.bind(client);
    this.fetchImpl = client.fetchImpl;
    this.baseUrl = client.baseUrl;
  }

  /** Reports whether Storvia can accept an upload, and why not when it cannot. */
  async status(options?: RequestOptions): Promise<StorageStatus> {
    return this.request<StorageStatus>('GET', '/v1/storage/status', { options });
  }

  /** Reads an object's metadata. */
  async getObject(objectId: string, options?: RequestOptions): Promise<StorviaObject> {
    return this.request<StorviaObject>(
      'GET',
      `/v1/storage/objects/${encodeURIComponent(objectId)}`,
      { options },
    );
  }

  /**
   * Removes an object from its recorded location.
   *
   * The deletion is pinned to wherever the file was actually written. A file
   * uploaded during a Drive outage is deleted from the fallback, because that is
   * where the only copy lives.
   */
  async deleteObject(objectId: string, options?: RequestOptions): Promise<void> {
    return this.request<void>(
      'DELETE',
      `/v1/storage/objects/${encodeURIComponent(objectId)}`,
      { options },
    );
  }

  /**
   * Streams an object's content.
   *
   * There is no second location to fall back to, so a provider outage surfaces as
   * an error rather than as a transparent retry. The caller owns the response body.
   */
  async download(
    objectId: string,
    range?: { start: number; end?: number },
    options?: RequestOptions,
  ): Promise<{ response: Response; mimeType: string; sizeBytes: number }> {
    const headers: Record<string, string> = {};
    if (range && range.start >= 0) {
      // HTTP ranges are inclusive at the end.
      headers['Range'] =
        range.end !== undefined ? `bytes=${range.start}-${range.end - 1}` : `bytes=${range.start}-`;
    }

    let response: Response;
    try {
      response = await this.fetchImpl(
        `${this.baseUrl}/v1/storage/objects/${encodeURIComponent(objectId)}/content`,
        { method: 'GET', headers, credentials: 'include', signal: options?.signal },
      );
    } catch (cause) {
      throw new TransportError('storvia: the download did not start', { cause });
    }

    if (!response.ok) {
      throw await this.decodeError(response);
    }

    const sizeHeader = response.headers.get('Content-Length');
    return {
      response,
      mimeType: response.headers.get('Content-Type') ?? 'application/octet-stream',
      sizeBytes: sizeHeader ? Number.parseInt(sizeHeader, 10) : -1,
    };
  }

  /** Turns an error response into a StorviaError. */
  private async decodeError(response: Response): Promise<StorviaError> {
    let payload: Problem | undefined;
    try {
      const text = await response.text();
      payload = text ? (JSON.parse(text) as { error?: Problem }).error : undefined;
    } catch {
      payload = undefined;
    }

    if (!payload) {
      return new StorviaError({
        kind: kindForStatus(response.status),
        status: response.status,
        title: response.statusText || 'Request failed',
        detail: 'the API returned a response that could not be parsed',
        requestId: response.headers.get('X-Request-ID') ?? undefined,
      });
    }

    return new StorviaError({
      kind: kindFromProblemType(payload.type),
      status: payload.status || response.status,
      title: payload.title,
      detail: payload.detail,
      requestId: payload.request_id ?? response.headers.get('X-Request-ID') ?? undefined,
      fields: payload.fields,
    });
  }
}

import { StorviaError, TransportError } from './core.ts';
import { kindFromProblemType, kindForStatus } from './core.ts';
import type { StorviaClient } from './core.ts';
