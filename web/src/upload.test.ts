import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { UploadOptions } from 'tus-js-client';
import { APIError } from './api';
import type { Attempt, PublicLink } from './types';

const tus = vi.hoisted(() => ({
  options: [] as UploadOptions[],
  events: [] as string[],
  hold: false,
}));
vi.mock('tus-js-client', () => ({
  Upload: class {
    options: UploadOptions;
    constructor(_file: File, options: UploadOptions) { this.options = options; tus.options.push(options); }
    start() {
      tus.events.push('tus:HEAD');
      if (!tus.hold) queueMicrotask(() => {
        tus.events.push('tus:PATCH');
        this.options.onProgress?.(5, 5);
        this.options.onSuccess?.({ lastResponse: {} as never });
      });
    }
    async abort(terminate: boolean) { expect(terminate).toBe(false); tus.events.push('tus:abort'); }
  },
}));
import { UploadQueue, shouldRetryTus, validateFile } from './upload';

const info: PublicLink = {
  id: 'a1', title: 'A request', instructions: '', max_file_bytes: 100,
  chunk_bytes: 8, expires_at: 2000000000, session_expires_at: 2000000000,
  busy: false, reset_available: false,
};
const receipt = (id = 'b1', status: Attempt['status'] = 'uploading'): Attempt => ({
  id, status, size: 5, offset: 0, upload_url: `https://drop.test/api/links/a1/uploads/${id}`, created_at: 1900000000,
});
const response = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
const file = (name = 'report.txt') => new File(['hello'], name);
beforeEach(() => {
  tus.options.length = 0; tus.events.length = 0; tus.hold = false;
  vi.stubGlobal('window', { location: { origin: 'https://drop.test' } });
});
afterEach(() => { vi.unstubAllGlobals(); vi.useRealTimers(); });

describe('serial upload queue', () => {
  it('admits one file at a time and verifies receipts before declaring completion', async () => {
    let admissions = 0;
    const fetch = vi.fn(async (path: string) => {
      if (path.endsWith('/attempts')) {
        admissions++; tus.events.push(`admit:${admissions}`);
        return response(receipt(`b${admissions}`));
      }
      tus.events.push(`receipt:${admissions}`);
      return response(receipt(`b${admissions}`, 'completed'));
    });
    vi.stubGlobal('fetch', fetch);
    const queue = new UploadQueue(info);
    queue.add([file('a.txt'), file('b.txt')]);
    await queue.start();
    expect(tus.events).toEqual(['admit:1', 'tus:HEAD', 'tus:PATCH', 'receipt:1', 'admit:2', 'tus:HEAD', 'tus:PATCH', 'receipt:2']);
    expect(queue.snapshot().items.every((item) => item.status === 'completed')).toBe(true);
    expect(tus.options[0]).toMatchObject({ storeFingerprintForResuming: false, headers: { 'X-Upfile-Request': '1' } });
    expect(tus.options[0].endpoint).toBeUndefined();
    expect(tus.options[0].metadata).toBeUndefined();
    expect(tus.options[0].parallelUploads).toBeUndefined();
  });
  it('retries a lost admission response with exactly the same key and payload', async () => {
    vi.useFakeTimers();
    let calls = 0;
    const bodies: string[] = [];
    vi.stubGlobal('fetch', vi.fn(async (path: string, init: RequestInit) => {
      if (path.endsWith('/attempts')) {
        bodies.push(init.body as string);
        if (calls++ === 0) throw new TypeError('Lost response');
        return response(receipt());
      }
      return response(receipt('b1', 'completed'));
    }));
    const queue = new UploadQueue(info);
    queue.add([file()]);
    const run = queue.start();
    await vi.runAllTimersAsync(); await run;
    expect(bodies.length).toBe(2);
    expect(bodies[0]).toBe(bodies[1]);
    expect(queue.snapshot().items[0].status).toBe('completed');
  });
  it('preserves successes, continues file validation failures, and stops on quota', async () => {
    let admissions = 0;
    vi.stubGlobal('fetch', vi.fn(async (path: string) => {
      if (path.endsWith('/attempts')) {
        if (++admissions === 2) return response({ error: 'Storage is full', code: 'storage_full' }, 507);
        return response(receipt());
      }
      return response(receipt('b1', 'completed'));
    }));
    const queue = new UploadQueue(info);
    queue.add([file('ok.txt'), new File(['x'.repeat(101)], 'large.txt'), file('quota.txt'), file('waiting.txt')]);
    await queue.start();
    expect(queue.snapshot().items.map((item) => item.status)).toEqual(['completed', 'failed', 'failed', 'queued']);
    expect(admissions).toBe(2);
  });
  it('aborts tus before calling the application cancel endpoint, never tus DELETE', async () => {
    tus.hold = true;
    let canceled = false;
    vi.stubGlobal('fetch', vi.fn(async (path: string, init: RequestInit) => {
      expect(init.method).not.toBe('DELETE');
      if (path.endsWith('/cancel')) { tus.events.push('app:cancel'); canceled = true; return response({ status: 'canceled' }); }
      return response(receipt('b1', canceled ? 'canceled' : 'uploading'));
    }));
    const queue = new UploadQueue(info);
    queue.add([file()]);
    const run = queue.start();
    await vi.waitFor(() => expect(tus.options).toHaveLength(1));
    await queue.cancel(queue.snapshot().items[0].id);
    await run;
    expect(tus.events.indexOf('tus:abort')).toBeLessThan(tus.events.indexOf('app:cancel'));
    expect(queue.snapshot().items[0].status).toBe('canceled');
  });
  it('does not mistake tus success for durable completion', async () => {
    vi.useFakeTimers();
    vi.stubGlobal('fetch', vi.fn(async (path: string) => response(receipt('b1', path.endsWith('/attempts') ? 'uploading' : 'finalizing'))));
    const queue = new UploadQueue(info);
    queue.add([file()]);
    const run = queue.start();
    await vi.runAllTimersAsync(); await run;
    expect(queue.snapshot().items[0].status).toBe('failed');
    expect(queue.snapshot().items[0].message).toContain('Receipt is not yet confirmed');
  });
  it('recovers an already completed admission without starting tus again', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => response(receipt('b1', 'completed'))));
    const queue = new UploadQueue(info);
    queue.add([file()]);
    await queue.start();
    expect(tus.options).toHaveLength(0);
    expect(queue.snapshot().items[0].status).toBe('completed');
  });
  it('blocks non-HEAD/PATCH requests and cross-origin upload addresses', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => response({ ...receipt(), upload_url: 'https://other.test/upload' })));
    const queue = new UploadQueue(info);
    queue.add([file()]);
    await queue.start();
    expect(tus.options).toHaveLength(0);
    expect(queue.snapshot().items[0].message).toContain('invalid upload address');
  });
  it('keeps admitted comments immutable and does not remove an unresolved attempt', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => response({ error: 'No space', code: 'storage_full' }, 507)));
    const queue = new UploadQueue(info);
    queue.add([file()]);
    const id = queue.snapshot().items[0].id;
    queue.comment(id, 'original');
    await queue.start();
    queue.retry(id);
    queue.comment(id, 'changed');
    queue.remove(id);
    expect(queue.snapshot().items[0].comment).toBe('original');
    expect(queue.snapshot().items).toHaveLength(1);
  });
  it('coalesces concurrent starts without duplicate admissions or transfers', async () => {
    let release!: (response: Response) => void;
    let admissions = 0;
    vi.stubGlobal('fetch', vi.fn(async (path: string) => {
      if (path.endsWith('/attempts')) {
        admissions++;
        return new Promise<Response>((resolve) => { release = resolve; });
      }
      return response(receipt('b1', 'completed'));
    }));
    const queue = new UploadQueue(info);
    queue.add([file()]);
    const first = queue.start();
    await queue.start();
    expect(admissions).toBe(1);
    release(response(receipt()));
    await first;
    expect(tus.options).toHaveLength(1);
    expect(queue.snapshot().items[0].status).toBe('completed');
  });
  it('cancels an in-flight admission before starting tus and cancels waiting files locally', async () => {
    let release!: (response: Response) => void;
    let canceled = false;
    let admissions = 0;
    vi.stubGlobal('fetch', vi.fn(async (path: string) => {
      if (path.endsWith('/attempts')) {
        admissions++;
        return new Promise<Response>((resolve) => { release = resolve; });
      }
      if (path.endsWith('/cancel')) {
        canceled = true;
        return response({ status: 'canceled' });
      }
      return response(receipt('b1', canceled ? 'canceled' : 'uploading'));
    }));
    const queue = new UploadQueue(info);
    queue.add([file('first.txt'), file('waiting.txt')]);
    const run = queue.start();
    await queue.cancelAll();
    release(response(receipt()));
    await run;
    expect(admissions).toBe(1);
    expect(tus.options).toHaveLength(0);
    expect(queue.snapshot().items.map((item) => item.status)).toEqual(['canceled', 'canceled']);
  });
  it('preserves completion when the server finalizes during cancellation', async () => {
    tus.hold = true;
    let cancelRequested = false;
    vi.stubGlobal('fetch', vi.fn(async (path: string) => {
      if (path.endsWith('/cancel')) {
        cancelRequested = true;
        return response({ status: 'canceled' });
      }
      return response(receipt('b1', cancelRequested ? 'completed' : 'uploading'));
    }));
    const queue = new UploadQueue(info);
    queue.add([file()]);
    const run = queue.start();
    await vi.waitFor(() => expect(tus.options).toHaveLength(1));
    await queue.cancel(queue.snapshot().items[0].id);
    await run;
    expect(queue.snapshot().items[0]).toMatchObject({ status: 'completed', sent: 5, message: 'Received before cancellation.' });
  });
  it('does not claim canceled or start the next file when cancellation fails', async () => {
    tus.hold = true;
    let admissions = 0;
    vi.stubGlobal('fetch', vi.fn(async (path: string) => {
      if (path.endsWith('/attempts')) admissions++;
      if (path.endsWith('/cancel')) return response({ code: 'session_required', error: 'Session expired' }, 401);
      return response(receipt());
    }));
    const queue = new UploadQueue(info);
    queue.add([file('first.txt'), file('waiting.txt')]);
    const run = queue.start();
    await vi.waitFor(() => expect(tus.options).toHaveLength(1));
    await queue.cancel(queue.snapshot().items[0].id);
    await run;
    expect(queue.snapshot().items.map((item) => item.status)).toEqual(['failed', 'queued']);
    expect(queue.snapshot().items[0].message).toContain('Cancellation not confirmed');
    expect(admissions).toBe(1);
  });
  it('retains the admission key after retry exhaustion and an explicit same-upload retry', async () => {
    vi.useFakeTimers();
    const keys: string[] = [];
    let offline = true;
    vi.stubGlobal('fetch', vi.fn(async (path: string, init: RequestInit) => {
      if (path.endsWith('/attempts')) {
        keys.push(JSON.parse(init.body as string).key);
        if (offline) throw new TypeError('Network unavailable');
        return response(receipt('b1', 'completed'));
      }
      return response(receipt('b1', 'completed'));
    }));
    const queue = new UploadQueue(info);
    queue.add([file()]);
    const run = queue.start();
    await vi.runAllTimersAsync();
    await run;
    expect(queue.snapshot().items[0].status).toBe('failed');
    offline = false;
    queue.retry(queue.snapshot().items[0].id);
    await queue.start();
    expect(keys).toHaveLength(5);
    expect(new Set(keys).size).toBe(1);
    expect(queue.snapshot().items[0].status).toBe('completed');
  });
  it('retries an interrupted admission body with the same payload rather than allocating afresh', async () => {
    vi.useFakeTimers();
    const bodies: string[] = [];
    vi.stubGlobal('fetch', vi.fn(async (_path: string, init: RequestInit) => {
      bodies.push(init.body as string);
      const result = response(receipt('b1', 'completed'));
      if (bodies.length === 1) vi.spyOn(result, 'json').mockRejectedValue(new TypeError('Body interrupted'));
      return result;
    }));
    const queue = new UploadQueue(info);
    queue.add([file()]);
    const run = queue.start();
    await vi.runAllTimersAsync();
    await run;
    expect(bodies).toHaveLength(2);
    expect(bodies[0]).toBe(bodies[1]);
    expect(queue.snapshot().items[0].status).toBe('completed');
  });
});
describe('upload retry guard and validation', () => {
  it('only retries transient statuses and actual offset conflicts', () => {
    for (const status of [401, 403, 404, 410, 413, 423, 507]) {
      const error = Object.assign(new Error(), { originalResponse: { getStatus: () => status, getBody: () => '' } });
      expect(shouldRetryTus(error as never)).toBe(false);
    }
    expect(shouldRetryTus(new APIError('quota', 'storage_full', 503))).toBe(false);
    expect(shouldRetryTus(new APIError('offset', 'offset_conflict', 409))).toBe(true);
    expect(shouldRetryTus(new APIError('key conflict', 'idempotency_conflict', 409))).toBe(false);
    for (const status of [408, 429, 500, 503]) expect(shouldRetryTus(new APIError('retry', `http_${status}`, status))).toBe(true);
  });
  it('validates comments by UTF-8 bytes and caps each file, not the queue sum', () => {
    expect(validateFile(file(), '🙂'.repeat(512), 5)).toBe('');
    expect(validateFile(file(), '🙂'.repeat(513), 5)).toContain('2,048');
    expect(validateFile(file(), '', 4)).toContain('limit');
  });
});
