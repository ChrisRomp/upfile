import { createServer, type Server } from 'node:http';
import { once } from 'node:events';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { Upload, type DetailedError } from 'tus-js-client';
import { shouldRetryTus } from './upload';

const payload = Buffer.from('an admitted file, uploaded in bounded chunks');
const resourcePath = '/api/links/a1/uploads/b1';
interface ObservedRequest {
  method: string;
  path: string;
  offset?: string;
  length?: string;
  contentType?: string;
  requestHeader?: string;
  metadata?: string;
  body?: Buffer;
}

describe('real tus-js-client against an existing HTTP resource', () => {
  let server: Server;
  let url: string;
  let stored: Buffer;
  let requests: ObservedRequest[];
  let headStatus: number;
  let losePatchResponse: boolean;
  let lostResponseAtOffset: number | undefined;
  let conflictOnce: boolean;
  let errors: string[];
  let activeUpload: Upload | undefined;

  beforeEach(async () => {
    stored = Buffer.alloc(0);
    requests = [];
    errors = [];
    headStatus = 200;
    losePatchResponse = false;
    lostResponseAtOffset = undefined;
    conflictOnce = false;
    server = createServer((request, response) => {
      const entry: ObservedRequest = {
        method: request.method || '',
        path: request.url || '',
        offset: request.headers['upload-offset']?.toString(),
        length: request.headers['content-length']?.toString(),
        contentType: request.headers['content-type'],
        requestHeader: request.headers['x-upfile-request']?.toString(),
        metadata: request.headers['upload-metadata']?.toString(),
      };
      requests.push(entry);
      response.setHeader('Tus-Resumable', '1.0.0');
      if (entry.path !== resourcePath || !['HEAD', 'PATCH'].includes(entry.method)) {
        errors.push(`Unexpected request: ${entry.method} ${entry.path}`);
        response.writeHead(405).end();
        return;
      }
      if (entry.method === 'HEAD') {
        response.setHeader('Upload-Length', String(payload.length));
        response.setHeader('Upload-Offset', String(stored.length));
        response.writeHead(headStatus).end();
        return;
      }
      const chunks: Buffer[] = [];
      request.on('data', (chunk: Buffer) => chunks.push(chunk));
      request.on('end', () => {
        const body = Buffer.concat(chunks);
        entry.body = body;
        if (entry.offset !== String(stored.length)) {
          errors.push(`Offset mismatch: ${entry.offset} != ${stored.length}`);
          response.writeHead(409).end();
          return;
        }
        if (conflictOnce) {
          conflictOnce = false;
          response.writeHead(409, { 'Content-Type': 'application/json' })
            .end(JSON.stringify({ code: 'offset_conflict', error: 'Retry with current offset.' }));
          return;
        }
        stored = Buffer.concat([stored, body]);
        if (losePatchResponse) {
          losePatchResponse = false;
          lostResponseAtOffset = stored.length;
          request.socket.destroy();
          return;
        }
        response.setHeader('Upload-Offset', String(stored.length));
        response.writeHead(204).end();
      });
      request.on('error', (error) => errors.push(error.message));
    });
    server.listen(0, '127.0.0.1');
    await once(server, 'listening');
    const address = server.address();
    if (!address || typeof address === 'string') throw new Error('Expected a local TCP address.');
    url = `http://127.0.0.1:${address.port}${resourcePath}`;
  });

  afterEach(async () => {
    await activeUpload?.abort(false);
    activeUpload = undefined;
    server.closeAllConnections();
    await new Promise<void>((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
  });

  function upload(chunkSize = 8): Promise<void> {
    return new Promise((resolve, reject) => {
      activeUpload = new Upload(payload, {
        uploadUrl: url,
        chunkSize,
        storeFingerprintForResuming: false,
        headers: { 'X-Upfile-Request': '1' },
        retryDelays: [0, 1, 2],
        onShouldRetry: shouldRetryTus,
        onSuccess: () => resolve(),
        onError: reject,
      });
      activeUpload.start();
    });
  }

  function verifyRequests() {
    expect(errors).toEqual([]);
    expect(requests.length).toBeGreaterThan(0);
    expect(requests.every((request) => ['HEAD', 'PATCH'].includes(request.method) && request.path === resourcePath)).toBe(true);
    for (const request of requests) {
      expect(request.requestHeader).toBe('1');
      expect(request.metadata).toBeUndefined();
      if (request.method === 'PATCH') {
        expect(Number(request.length)).toBe(request.body?.length);
        expect(request.contentType).toBe('application/offset+octet-stream');
      }
    }
  }

  it('uses HEAD followed by bounded PATCHes without a creation endpoint or POST', async () => {
    await upload();
    expect(requests[0].method).toBe('HEAD');
    expect(requests.filter((request) => request.method === 'PATCH').every((request) => Number(request.length) <= 8)).toBe(true);
    expect(stored).toEqual(payload);
    verifyRequests();
  });

  it('recovers an existing nonzero offset without resending committed bytes', async () => {
    stored = payload.subarray(0, 11);
    await upload();
    expect(requests.find((request) => request.method === 'PATCH')?.offset).toBe('11');
    expect(stored).toEqual(payload);
    verifyRequests();
  });

  it('recovers a lost PATCH response using HEAD and the committed offset', async () => {
    losePatchResponse = true;
    await upload();
    expect(lostResponseAtOffset).toBe(8);
    expect(requests.slice(0, 4).map((request) => request.method)).toEqual(['HEAD', 'PATCH', 'HEAD', 'PATCH']);
    expect(requests[3].offset).toBe('8');
    expect(stored).toEqual(payload);
    verifyRequests();
  });

  it('recovers a lost final response with a full-offset HEAD, without uploading again', async () => {
    losePatchResponse = true;
    await upload(payload.length);
    expect(requests.map((request) => request.method)).toEqual(['HEAD', 'PATCH', 'HEAD']);
    expect(lostResponseAtOffset).toBe(payload.length);
    expect(stored).toEqual(payload);
    verifyRequests();
  });

  it('accepts an already completed resource without PATCH or POST', async () => {
    stored = payload;
    await upload();
    expect(requests.map((request) => request.method)).toEqual(['HEAD']);
    verifyRequests();
  });

  it('resolves an offset conflict by checking HEAD on the same resource', async () => {
    conflictOnce = true;
    await upload();
    expect(requests.slice(0, 4).map((request) => request.method)).toEqual(['HEAD', 'PATCH', 'HEAD', 'PATCH']);
    expect(stored).toEqual(payload);
    verifyRequests();
  });

  it.each([401, 403, 404, 410, 413, 423, 507])('never retries or creates a resource after terminal HEAD %s', async (status) => {
    headStatus = status;
    const result: Promise<DetailedError> = upload().then(
      () => { throw new Error('Expected upload to fail.'); },
      (error: DetailedError) => error,
    );
    expect((await result).originalResponse?.getStatus()).toBe(status);
    expect(requests.map((request) => request.method)).toEqual(['HEAD']);
    expect(stored.length).toBe(0);
    verifyRequests();
  });

  it('bounds retries for a persistently unavailable resource without creation fallback', async () => {
    headStatus = 503;
    await expect(upload()).rejects.toHaveProperty('originalResponse');
    expect(requests.map((request) => request.method)).toEqual(['HEAD', 'HEAD', 'HEAD', 'HEAD']);
    expect(stored.length).toBe(0);
    verifyRequests();
  });
});
