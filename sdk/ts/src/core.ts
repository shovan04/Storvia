/**
 * Storvia API client core for TypeScript and JavaScript.
 *
 * Defines the base client, error types, and the shared request transport.
 * Domain-specific clients (upload, storage, drive) are composed into
 * {@link StorviaClient} and exposed as properties.
 */

/** Machine-readable failure class, matching the server's problem types. */
export type ErrorKind =
  | 'validation'
  | 'authentication'
  | 'authorization'
  | 'not_found'
  | 'conflict'
  | 'quota'
  | 'rate_limited'
  | 'unavailable'
  | 'uncertain'
  | 'provider_not_configured'
  | 'consent_required'
  | 'internal';

/** RFC 9457 problem details, as returned for every failure. */
export interface Problem {
  type: string;
  title: string;
  status: number;
  detail: string;
  instance?: string;
  request_id?: string;
  fields?: Record<string, string>;
}

/** An error returned by the Storvia API. */
export class StorviaError extends Error {
  readonly kind: ErrorKind;
  readonly status: number;
  readonly title: string;
  readonly detail: string;
  readonly requestId: string;
  readonly fields: Record<string, string>;

  constructor(init: {
    kind: ErrorKind;
    status: number;
    title: string;
    detail: string;
    requestId?: string;
    fields?: Record<string, string>;
  }) {
    super(`${init.kind}: ${init.detail}`);
    this.name = 'StorviaError';
    this.kind = init.kind;
    this.status = init.status;
    this.title = init.title;
    this.detail = init.detail;
    this.requestId = init.requestId ?? '';
    this.fields = init.fields ?? {};
  }

  /**
   * Whether retrying the identical call could succeed.
   *
   * Quota, consent, and validation failures return false: retrying them burns the
   * user's quota or their patience without changing the outcome.
   */
  get temporary(): boolean {
    return (
      this.kind === 'unavailable' ||
      this.kind === 'rate_limited' ||
      this.kind === 'uncertain' ||
      this.kind === 'internal'
    );
  }

  /**
   * Whether the request may or may not have been processed.
   *
   * This is the dangerous case: the write may have committed. Retry the same
   * operation with the same idempotency key, or reconcile; do not start a new
   * upload.
   */
  get outcomeUnknown(): boolean {
    return this.kind === 'uncertain';
  }

  /** Whether a reconnect of the Drive account is what the user needs to do. */
  get needsReconnect(): boolean {
    return this.kind === 'consent_required';
  }
}

/** A failure raised before a response was received. */
export class TransportError extends Error {
  /** Always true: a transport failure cannot prove the server did not act. */
  readonly outcomeUnknown = true;

  constructor(message: string, options?: { cause?: unknown }) {
    super(message, options);
    this.name = 'TransportError';
  }
}

const PROBLEM_PREFIX = 'https://storvia.dev/problems/';

const KNOWN_KINDS: ReadonlySet<ErrorKind> = new Set<ErrorKind>([
  'validation',
  'authentication',
  'authorization',
  'not_found',
  'conflict',
  'quota',
  'rate_limited',
  'unavailable',
  'uncertain',
  'provider_not_configured',
  'consent_required',
  'internal',
]);

/**
 * Maps a problem type to a known kind.
 *
 * An unregistered type degrades to `internal` rather than being trusted verbatim:
 * a caller branching on a kind it does not understand would take the wrong action.
 */
export function kindFromProblemType(type: string): ErrorKind {
  if (!type.startsWith(PROBLEM_PREFIX)) {
    return 'internal';
  }
  const kind = type.slice(PROBLEM_PREFIX.length);
  const kindAsErrorKind = kind as ErrorKind;
  return KNOWN_KINDS.has(kindAsErrorKind) ? kindAsErrorKind : 'internal';
}

/** Maps a status to a kind when no problem document is available. */
export function kindForStatus(status: number): ErrorKind {
  switch (status) {
    case 400:
    case 422:
      return 'validation';
    case 401:
      return 'authentication';
    case 403:
      return 'authorization';
    case 404:
      return 'not_found';
    case 409:
      return 'conflict';
    case 428:
      return 'consent_required';
    case 424:
      return 'provider_not_configured';
    case 429:
      return 'rate_limited';
    case 503:
      return 'unavailable';
    default:
      return 'internal';
  }
}

/** Options for the client. */
export interface StorviaClientOptions {
  /** API root, for example `https://api.storvia.example`. */
  baseUrl: string;
  /** Session credential. Omit when the browser relies on the session cookie. */
  token?: string;
  /** Custom fetch implementation, mainly for tests and server runtimes. */
  fetch?: typeof fetch;
  /** Extra headers applied to every request. */
  headers?: Record<string, string>;
}

/** Options for a single request. */
export interface RequestOptions {
  /** Abort the request. */
  signal?: AbortSignal;
  /** Stable across retries of the same logical request. */
  idempotencyKey?: string;
}

/** The base client: configuration, transport, and error handling. */
export class StorviaClient {
  readonly baseUrl: string;
  readonly token: string | undefined;
  readonly fetchImpl: typeof fetch;
  readonly extraHeaders: Record<string, string>;

  /** Domain clients, composed into the base client. Set by createStorviaClient(). */
  upload?: import('./upload.ts').UploadClient;
  storage?: import('./storage.ts').StorageClient;
  drive?: import('./drive.ts').DriveClient;

  constructor(options: StorviaClientOptions) {
    if (!options.baseUrl) {
      throw new Error('storvia: a baseUrl is required');
    }
    const parsed = new URL(options.baseUrl);
    if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
      throw new Error(`storvia: baseUrl must be http or https, got ${parsed.protocol}`);
    }
    this.baseUrl = parsed.toString().replace(/\/$/, '');
    this.token = options.token;
    this.fetchImpl = options.fetch ?? globalThis.fetch;
    this.extraHeaders = options.headers ?? {};
  }

  /** Performs a request and decodes the response. Domain clients call this. */
  public async request<T>(
    method: string,
    path: string,
    init: {
      body?: unknown;
      raw?: BodyInit;
      contentType?: string;
      options?: RequestOptions;
    } = {},
  ): Promise<T> {
    const headers: Record<string, string> = {
      Accept: 'application/json',
      ...this.extraHeaders,
    };
    if (this.token) {
      headers['Authorization'] = `Bearer ${this.token}`;
    }
    if (init.options?.idempotencyKey) {
      headers['Idempotency-Key'] = init.options.idempotencyKey;
    }

    let body: BodyInit | undefined;
    if (init.raw !== undefined) {
      body = init.raw;
      headers['Content-Type'] = init.contentType ?? 'application/octet-stream';
    } else if (init.body !== undefined) {
      body = JSON.stringify(init.body);
      headers['Content-Type'] = 'application/json';
    }

    let response: Response;
    try {
      response = await this.fetchImpl(`${this.baseUrl}${path}`, {
        method,
        headers,
        body,
        credentials: 'include',
        signal: init.options?.signal,
      });
    } catch (cause) {
      throw new TransportError('storvia: the request did not complete', { cause });
    }

    if (!response.ok) {
      throw await this.decodeError(response);
    }

    if (response.status === 204) {
      return undefined as T;
    }
    const text = await response.text();
    if (!text) {
      return undefined as T;
    }
    return JSON.parse(text) as T;
  }

  /** Turns an error response into a StorviaError. Domain clients call this. */
  protected async decodeError(response: Response): Promise<StorviaError> {
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

  // ---- Delegated methods (set by createStorviaClient) ----

  /** Reports whether Storvia can accept an upload, and why not when it cannot. */
  status(options?: RequestOptions): Promise<import('./storage.ts').StorageStatus> {
    if (!this.storage) {
      throw new Error('storvia: storage client not initialized. Use createStorviaClient().');
    }
    return this.storage.status(options);
  }

  /** Starts a new upload. */
  async createUpload(
    request: import('./upload.ts').CreateUploadRequest,
    options?: RequestOptions,
  ): Promise<import('./upload.ts').CreateUploadResult> {
    if (!this.upload) {
      throw new Error('storvia: upload client not initialized. Use createStorviaClient().');
    }
    return this.upload.createUpload(request, options);
  }

  /** Reads an upload's state. */
  getUpload(uploadId: string, options?: RequestOptions): Promise<import('./upload.ts').UploadView> {
    if (!this.upload) {
      throw new Error('storvia: upload client not initialized. Use createStorviaClient().');
    }
    return this.upload.getUpload(uploadId, options);
  }

  /** Abandons an upload that has not begun writing bytes. */
  cancelUpload(uploadId: string, options?: RequestOptions): Promise<void> {
    if (!this.upload) {
      throw new Error('storvia: upload client not initialized. Use createStorviaClient().');
    }
    return this.upload.cancelUpload(uploadId, options);
  }

  /** Finalizes an upload. */
  async completeUpload(
    uploadId: string,
    driveFileId?: string,
    options?: RequestOptions,
  ): Promise<import('./storage.ts').StorviaObject> {
    if (!this.upload) {
      throw new Error('storvia: upload client not initialized. Use createStorviaClient().');
    }
    return this.upload.completeUpload(uploadId, driveFileId, options);
  }

  /** Asks the server what actually happened to an unfinished upload. */
  async reconcileUpload(
    uploadId: string,
    options?: RequestOptions,
  ): Promise<import('./upload.ts').ReconcileResult> {
    if (!this.upload) {
      throw new Error('storvia: upload client not initialized. Use createStorviaClient().');
    }
    return this.upload.reconcileUpload(uploadId, options);
  }

  /** Reads an object's metadata. */
  getObject(objectId: string, options?: RequestOptions): Promise<import('./storage.ts').StorviaObject> {
    if (!this.storage) {
      throw new Error('storvia: storage client not initialized. Use createStorviaClient().');
    }
    return this.storage.getObject(objectId, options);
  }

  /** Removes an object from its recorded location. */
  deleteObject(objectId: string, options?: RequestOptions): Promise<void> {
    if (!this.storage) {
      throw new Error('storvia: storage client not initialized. Use createStorviaClient().');
    }
    return this.storage.deleteObject(objectId, options);
  }

  /** Streams an object's content. */
  async download(
    objectId: string,
    range?: { start: number; end?: number },
    options?: RequestOptions,
  ): Promise<{ response: Response; mimeType: string; sizeBytes: number }> {
    if (!this.storage) {
      throw new Error('storvia: storage client not initialized. Use createStorviaClient().');
    }
    return this.storage.download(objectId, range, options);
  }

  /** Reads the Drive connection state. */
  getDriveConnection(options?: RequestOptions): Promise<import('./drive.ts').DriveConnection> {
    if (!this.drive) {
      throw new Error('storvia: drive client not initialized. Use createStorviaClient().');
    }
    return this.drive.getDriveConnection(options);
  }

  /** Begins the Drive consent flow. */
  async startDriveConnection(options?: RequestOptions): Promise<string> {
    if (!this.drive) {
      throw new Error('storvia: drive client not initialized. Use createStorviaClient().');
    }
    return this.drive.startDriveConnection(options);
  }

  /** Removes the Drive connection. */
  disconnectDrive(options?: RequestOptions): Promise<void> {
    if (!this.drive) {
      throw new Error('storvia: drive client not initialized. Use createStorviaClient().');
    }
    return this.drive.disconnectDrive(options);
  }
}
