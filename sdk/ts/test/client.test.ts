/**
 * Tests for the Storvia TypeScript SDK.
 *
 * They run against a stub fetch rather than a live API, so they assert the wire
 * contract: the idempotency header, the RFC 9457 decoding, the unknown-outcome
 * classification, and the refusal to guess at provider selection.
 */

import assert from 'node:assert/strict';
import { describe, it } from 'node:test';

import {
  createStorviaClient,
  StorviaClient,
  StorviaError,
  TransportError,
} from '../src/index.ts';

/** Builds a client backed by a stub fetch that records what it was asked to send. */
function stubClient(
  respond: (url: string, init: RequestInit) => Response,
): { client: StorviaClient; calls: Array<{ url: string; init: RequestInit }> } {
  const calls: Array<{ url: string; init: RequestInit }> = [];
  const stubFetch = (async (url: string | URL | Request, init: RequestInit = {}) => {
    const href = typeof url === 'string' ? url : url.toString();
    calls.push({ url: href, init });
    return respond(href, init);
  }) as typeof fetch;

  const client = createStorviaClient({
    baseUrl: 'https://api.example.test',
    token: 'test-token',
    fetch: stubFetch,
  });
  return { client, calls };
}

/** Builds a JSON response carrying an RFC 9457 problem. */
function problemResponse(
  status: number,
  kind: string,
  detail: string,
  requestId = 'corr-1',
): Response {
  return new Response(
    JSON.stringify({
      error: {
        type: `https://storvia.dev/problems/${kind}`,
        title: 'T',
        status,
        detail,
        request_id: requestId,
      },
    }),
    {
      status,
      headers: {
        'Content-Type': 'application/problem+json',
        'X-Request-ID': requestId,
      },
    },
  );
}

describe('StorviaClient configuration', () => {
  it('requires a base URL', () => {
    assert.throws(() => new StorviaClient({ baseUrl: '' }));
  });

  it('rejects a non-http scheme', () => {
    assert.throws(() => new StorviaClient({ baseUrl: 'ftp://example.com' }));
  });

  it('normalizes a trailing slash', () => {
    const client = new StorviaClient({ baseUrl: 'https://api.example.test/' });
    assert.equal(client['baseUrl' as keyof StorviaClient], 'https://api.example.test');
  });
});

describe('createUpload', () => {
  it('sends the idempotency key', async () => {
    const { client, calls } = stubClient(
      () =>
        new Response(
          JSON.stringify({
            upload_id: 'upl_1',
            object_id: 'obj_1',
            mode: 'drive_resumable_session',
            provider: 'google_drive',
            state: 'uploading',
            chunk_size: 8388608,
            uploaded_bytes: 0,
            replayed: false,
          }),
          { status: 201, headers: { 'Content-Type': 'application/json' } },
        ),
    );

    const result = await client.createUpload(
      { name: 'report.pdf', mime_type: 'application/pdf', size_bytes: 2048, category: 'report' },
      { idempotencyKey: 'stable-key-1' },
    );

    const headers = calls[0]?.init.headers as Record<string, string>;
    assert.equal(headers['Idempotency-Key'], 'stable-key-1');
    assert.equal(headers['Authorization'], 'Bearer test-token');
    assert.equal(result.mode, 'drive_resumable_session');
    assert.equal(result.provider, 'google_drive');
  });

  it('refuses to send without an idempotency key', async () => {
    const { client } = stubClient(() => new Response('', { status: 500 }));
    await assert.rejects(() =>
      client.createUpload({
        name: 'a.pdf',
        mime_type: 'application/pdf',
        size_bytes: 1,
        category: 'report',
      }),
    );
  });

  it('surfaces an outage routing decision to the caller', async () => {
    const { client } = stubClient(
      () =>
        new Response(
          JSON.stringify({
            upload_id: 'upl_1',
            object_id: 'obj_1',
            mode: 's3_presigned_parts',
            provider: 's3_compatible',
            state: 'uploading',
            chunk_size: 8388608,
            uploaded_bytes: 0,
            replayed: false,
            fallback_reason: 'drive_preflight_timeout',
            parts: [{ part_number: 1, url: 'https://example.com/presigned', expires_at: '2030-01-01T00:00:00Z' }],
          }),
          { status: 201, headers: { 'Content-Type': 'application/json' } },
        ),
    );

    const result = await client.createUpload(
      { name: 'report.pdf', mime_type: 'application/pdf', size_bytes: 1 << 20, category: 'report' },
      { idempotencyKey: 'k' },
    );

    assert.equal(result.mode, 's3_presigned_parts');
    assert.equal(result.fallback_reason, 'drive_preflight_timeout');
    assert.equal(result.parts?.length, 1);
  });
});

describe('error handling', () => {
  it('decodes the problem type into a machine-readable kind', async () => {
    const cases: Array<[number, string]> = [
      [507, 'quota'],
      [503, 'unavailable'],
      [409, 'uncertain'],
      [428, 'consent_required'],
      [409, 'conflict'],
      [400, 'validation'],
    ];

    for (const [status, kind] of cases) {
      const { client } = stubClient(() => problemResponse(status, kind, `${kind} detail`));
      await assert.rejects(
        () => client.getObject('obj_1'),
        (error: unknown) => {
          assert.ok(error instanceof StorviaError);
          assert.equal(error.kind, kind);
          assert.equal(error.status, status);
          assert.equal(error.requestId, 'corr-1');
          return true;
        },
      );
    }
  });

  it('marks an uncertain outcome as retryable and unknown', async () => {
    const { client } = stubClient(() => problemResponse(409, 'uncertain', 'the write result cannot be proved'));
    await assert.rejects(
      () => client.getObject('obj_1'),
      (error: unknown) => {
        assert.ok(error instanceof StorviaError);
        assert.equal(error.outcomeUnknown, true);
        assert.equal(error.temporary, true);
        return true;
      },
    );
  });

  it('does not mark a quota error retryable', async () => {
    const { client } = stubClient(() => problemResponse(507, 'quota', 'drive storage is full'));
    await assert.rejects(
      () => client.getObject('obj_1'),
      (error: unknown) => {
        assert.ok(error instanceof StorviaError);
        assert.equal(error.temporary, false);
        assert.equal(error.outcomeUnknown, false);
        return true;
      },
    );
  });

  it('flags a consent failure as needing a reconnect', async () => {
    const { client } = stubClient(() => problemResponse(428, 'consent_required', 'reconnect'));
    await assert.rejects(
      () => client.getObject('obj_1'),
      (error: unknown) => {
        assert.ok(error instanceof StorviaError);
        assert.equal(error.needsReconnect, true);
        return true;
      },
    );
  });

  it('degrades an unknown problem type to internal', async () => {
    const { client } = stubClient(
      () =>
        new Response(
          JSON.stringify({
            error: {
              type: 'https://storvia.dev/problems/some_new_thing',
              title: 'T',
              status: 403,
              detail: 'D',
            },
          }),
          { status: 403, headers: { 'Content-Type': 'application/problem+json' } },
        ),
    );

    await assert.rejects(
      () => client.getObject('obj_1'),
      (error: unknown) => {
        assert.ok(error instanceof StorviaError);
        // Guessing a kind this SDK does not understand would be worse than
        // reporting the conservative bucket.
        assert.equal(error.kind, 'internal');
        return true;
      },
    );
  });

  it('does not surface a non-JSON upstream body', async () => {
    const { client } = stubClient(
      () => new Response('<html>proxy error naming internal-host-9.9.9.9</html>', { status: 502 }),
    );

    await assert.rejects(
      () => client.getObject('obj_1'),
      (error: unknown) => {
        assert.ok(error instanceof StorviaError);
        assert.ok(!error.detail.includes('internal-host-9.9.9.9'));
        return true;
      },
    );
  });

  it('treats a transport failure as an unknown outcome', async () => {
    const client = createStorviaClient({
      baseUrl: 'https://api.example.test',
      fetch: (async () => {
        throw new Error('socket hang up');
      }) as typeof fetch,
    });

    await assert.rejects(
      () => client.getObject('obj_1'),
      (error: unknown) => {
        assert.ok(error instanceof TransportError);
        // The write may have committed; retrying it as a fresh request is what
        // creates duplicates.
        assert.equal(error.outcomeUnknown, true);
        return true;
      },
    );
  });
});

describe('status and connections', () => {
  it('exposes the fallback disclosure', async () => {
    const { client } = stubClient(
      () =>
        new Response(
          JSON.stringify({
            primary_provider: 'google_drive',
            drive_connected: true,
            fallback_enabled: true,
            fallback_vendor: 'cloudflare_r2',
            fallback_ready: true,
            fallback_requires_consent: false,
            accepts_uploads: true,
            fallback_stores_outside_drive: true,
          }),
          { status: 200, headers: { 'Content-Type': 'application/json' } },
        ),
    );

    const status = await client.status();
    // A UI must be able to tell the user their file may leave their Drive.
    assert.equal(status.fallback_stores_outside_drive, true);
    assert.equal(status.accepts_uploads, true);
  });

  it('returns the Drive consent URL', async () => {
    const { client } = stubClient(
      () =>
        new Response(
          JSON.stringify({ authorize_url: 'https://accounts.google.com/o/oauth2/v2/auth?x=1', expires_in_seconds: 600 }),
          { status: 201, headers: { 'Content-Type': 'application/json' } },
        ),
    );

    const url = await client.startDriveConnection();
    assert.ok(url.startsWith('https://accounts.google.com/'));
  });

  it('rejects an empty consent URL', async () => {
    const { client } = stubClient(
      () =>
        new Response(JSON.stringify({ authorize_url: '', expires_in_seconds: 0 }), {
          status: 201,
          headers: { 'Content-Type': 'application/json' } },
        ),
    );
    await assert.rejects(() => client.startDriveConnection());
  });
});

describe('download', () => {
  it('sends an inclusive range header', async () => {
    const { client, calls } = stubClient(
      () =>
        new Response('chunk', {
          status: 206,
          headers: { 'Content-Type': 'text/plain', 'Content-Length': '5' },
        }),
    );

    const { response, sizeBytes } = await client.download('obj_1', { start: 100, end: 200 });
    const headers = calls[0]?.init.headers as Record<string, string>;
    assert.equal(headers['Range'], 'bytes=100-199');
    assert.equal(response.status, 206);
    assert.equal(sizeBytes, 5);
  });

  it('surfaces a download failure as a Storvia error', async () => {
    const { client } = stubClient(() => problemResponse(503, 'unavailable', 'no second copy exists to serve from'));
    await assert.rejects(
      () => client.download('obj_1'),
      (error: unknown) => {
        assert.ok(error instanceof StorviaError);
        assert.equal(error.kind, 'unavailable');
        return true;
      },
    );
  });
});

describe('upload helper', () => {
  it('streams a Drive resumable session and completes with the returned file id', async () => {
    const ranges: string[] = [];
    const chunkSize = 60;
    const size = 100;
    let driveFileId = '';
    let completeBody: unknown;

    // Stand in for Google's resumable endpoint.
    const sessionUrl = 'https://www.googleapis.com/upload/drive/v3/files?upload_id=s1';
    globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      const headers = (init?.headers ?? {}) as Record<string, string>;

      if (url === sessionUrl) {
        const body = init?.body as Blob;
        ranges.push(headers['Content-Range']);
        // The chunk that reaches the end of the file returns the created resource;
        // earlier ones ask for more.
        if (headers['Content-Range'].endsWith(`/${size}`)) {
          return new Response(JSON.stringify({ id: 'file-1', name: 'a.bin' }), { status: 200 });
        }
        return new Response(null, { status: 308 });
      }

      if (url.endsWith('/complete')) {
        completeBody = JSON.parse(String(init?.body ?? '{}'));
        return new Response(
          JSON.stringify({
            object: {
              object_id: 'obj_1',
              name: 'a.bin',
              mime_type: 'application/octet-stream',
              size_bytes: size,
              status: 'ready',
              provider: 'google_drive',
              category: 'report',
              created_at: '',
            },
            already_complete: false,
          }),
          { status: 201, headers: { 'Content-Type': 'application/json' } },
        );
      }

      return new Response(
        JSON.stringify({
          upload_id: 'upl_1',
          object_id: 'obj_1',
          mode: 'drive_resumable_session',
          provider: 'google_drive',
          state: 'uploading',
          chunk_size: chunkSize,
          uploaded_bytes: 0,
          replayed: false,
          session: { url: sessionUrl, chunk_size: chunkSize, expires_at: '' },
        }),
        { status: 201, headers: { 'Content-Type': 'application/json' } },
      );
    }) as typeof fetch;

    const client = createStorviaClient({ baseUrl: 'https://storvia.test' });
    const { object } = await client.upload.upload(
      { name: 'a.bin', mime_type: 'application/octet-stream', size_bytes: size, category: 'report' },
      new Blob([new Uint8Array(size)]),
      { idempotencyKey: 'k' },
    );

    // Every chunk must declare its byte range, or a retry corrupts the file.
    assert.deepEqual(ranges, ['bytes 0-59/100', 'bytes 60-99/100']);
    assert.equal(object.status, 'ready');

    // Completion must carry the id Google returned, because Storvia does not keep
    // the session and cannot learn it another way.
    assert.deepEqual(completeBody, { drive_file_id: 'file-1' });
  });

  it('refuses to complete a Drive upload that produced no file id', async () => {
    const sessionUrl = 'https://www.googleapis.com/upload/drive/v3/files?upload_id=s2';
    globalThis.fetch = (async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url === sessionUrl) {
        // 308 all the way through: the file never completes.
        return new Response(null, { status: 308 });
      }
      return new Response(
        JSON.stringify({
          upload_id: 'upl_1',
          object_id: 'obj_1',
          mode: 'drive_resumable_session',
          provider: 'google_drive',
          state: 'uploading',
          chunk_size: 100,
          uploaded_bytes: 0,
          replayed: false,
          session: { url: sessionUrl, chunk_size: 100, expires_at: '' },
        }),
        { status: 201, headers: { 'Content-Type': 'application/json' } },
      );
    }) as typeof fetch;

    const client = createStorviaClient({ baseUrl: 'https://storvia.test' });
    await assert.rejects(
      () =>
        client.upload.upload(
          { name: 'a.bin', mime_type: 'application/octet-stream', size_bytes: 100, category: 'report' },
          new Blob([new Uint8Array(100)]),
          { idempotencyKey: 'k' },
        ),
      /did not return a file id/,
    );
  });

  it('reports a Drive rejection with the byte range it refused', async () => {
    const sessionUrl = 'https://www.googleapis.com/upload/drive/v3/files?upload_id=s3';
    globalThis.fetch = (async (input: RequestInfo | URL) => {
      if (String(input) === sessionUrl) {
        return new Response('nope', { status: 403 });
      }
      return new Response(
        JSON.stringify({
          upload_id: 'upl_1',
          object_id: 'obj_1',
          mode: 'drive_resumable_session',
          provider: 'google_drive',
          state: 'uploading',
          chunk_size: 100,
          uploaded_bytes: 0,
          replayed: false,
          session: { url: sessionUrl, chunk_size: 100, expires_at: '' },
        }),
        { status: 201, headers: { 'Content-Type': 'application/json' } },
      );
    }) as typeof fetch;

    const client = createStorviaClient({ baseUrl: 'https://storvia.test' });
    await assert.rejects(
      () =>
        client.upload.upload(
          { name: 'a.bin', mime_type: 'application/octet-stream', size_bytes: 100, category: 'report' },
          new Blob([new Uint8Array(100)]),
          { idempotencyKey: 'k' },
        ),
      /byte range 0-99\/100/,
    );
  });

  it('surfaces a reconcile outcome rather than assuming nothing landed', async () => {
    const { client } = stubClient(() =>
      new Response(
        JSON.stringify({
          upload_id: 'upl_1',
          object_id: 'obj_1',
          provider: 'google_drive',
          state: 'uploading',
          outcome: 'unknown',
          reason: 'storvia does not hold the drive upload session',
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      ),
    );

    const result = await client.reconcileUpload('upl_1');
    // "unknown" must not read as "safe to retry", or a client would duplicate.
    assert.equal(result.outcome, 'unknown');
    assert.ok(result.reason && result.reason.length > 0);
  });
});
