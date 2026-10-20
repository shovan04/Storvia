/**
 * Drive domain for the Storvia TypeScript SDK.
 *
 * Handles the Drive connection lifecycle: reading connection state, starting the
 * consent flow, and disconnecting. Composed into {@link StorviaClient}.
 */

/** A Drive connection, reported without any credential. */
export interface DriveConnection {
  connected: boolean;
  status: string;
  subject?: string;
  granted_scopes?: string[];
  folder_present: boolean;
  last_success_at?: string;
  stale: boolean;
  /** True when files are stored in Drive's hidden appDataFolder instead of a
   *  visible folder in My Drive. Files there are invisible to the user in Drive
   *  web and mobile. */
  uses_app_data_folder?: boolean;
}

/** Options for a single request. */
export interface RequestOptions {
  /** Abort the request. */
  signal?: AbortSignal;
  /** Stable across retries of the same logical request. */
  idempotencyKey?: string;
}

/** The drive domain client. Composed into StorviaClient. */
export class DriveClient {
  public request: StorviaClient['request'];

  constructor(client: StorviaClient) {
    this.request = client.request.bind(client);
  }

  /** Reads the Drive connection state. */
  async getDriveConnection(options?: RequestOptions): Promise<DriveConnection> {
    return this.request<DriveConnection>(
      'GET',
      '/v1/storage/connections/google-drive',
      { options },
    );
  }

  /**
   * Begins the Drive consent flow.
   *
   * Send the browser to the returned URL. It is shown once and should not be logged.
   */
  async startDriveConnection(options?: RequestOptions): Promise<string> {
    const result = await this.request<{
      authorize_url: string;
      expires_in_seconds: number;
    }>('POST', '/v1/storage/connections/google-drive/start', { options });
    if (!result.authorize_url) {
      throw new StorviaError({
        kind: 'internal',
        status: 502,
        title: 'Empty consent URL',
        detail: 'The server returned an empty consent URL.',
      });
    }
    return result.authorize_url;
  }

  /**
   * Removes the Drive connection.
   *
   * New Drive requests and new uploads stop, and the user's own Drive files are
   * left untouched.
   */
  async disconnectDrive(options?: RequestOptions): Promise<void> {
    return this.request<void>(
      'DELETE',
      '/v1/storage/connections/google-drive',
      { options },
    );
  }
}

import { StorviaError } from './core.ts';
import type { StorviaClient } from './core.ts';
