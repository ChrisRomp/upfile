import { afterEach, describe, expect, it, vi } from 'vitest';
import { readFileSync } from 'node:fs';
import { runInNewContext } from 'node:vm';
import { api, APIError, retryable, withRetry } from './api';
import { bytes, localDateTime, utf8Length } from './format';

afterEach(() => { vi.unstubAllGlobals(); vi.useRealTimers(); });
describe('same-origin API', () => {
  it('sends mutations with JSON, credentials and the request header', async () => {
    const fetch = vi.fn().mockResolvedValue(new Response('{"status":"ok"}'));
    vi.stubGlobal('fetch', fetch);
    await api('/api/settings', 'PUT', { max_file_bytes: 12 });
    expect(fetch).toHaveBeenCalledWith('/api/settings', expect.objectContaining({
      credentials: 'same-origin', method: 'PUT', body: '{"max_file_bytes":12}',
      headers: expect.objectContaining({ 'X-Upfile-Request': '1', 'Content-Type': 'application/json' }),
    }));
  });
  it('surfaces structured errors and unexpected login HTML', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(new Response('{"code":"storage_full","error":"No space"}', { status: 507 }))
      .mockResolvedValueOnce(new Response('<html>Sign in</html>')));
    await expect(api('/api/links/abc/attempts', 'POST', {})).rejects.toMatchObject({ code: 'storage_full', message: 'No space' });
    await expect(api('/api/settings')).rejects.toMatchObject({ code: 'invalid_response' });
  });
  it('never retries quota, session or validation failures regardless of HTTP status', () => {
    for (const code of ['storage_full', 'record_limit', 'session_required', 'too_large', 'idempotency_conflict', 'busy']) {
      expect(retryable(new APIError('failure', code, 503))).toBe(false);
    }
  });
  it('bounds transient retries to three', async () => {
    vi.useFakeTimers();
    const action = vi.fn().mockRejectedValue(new APIError('offline', 'network'));
    const promise = withRetry(action);
    const assertion = expect(promise).rejects.toMatchObject({ code: 'network' });
    await vi.runAllTimersAsync();
    await assertion;
    expect(action).toHaveBeenCalledTimes(4);
  });
  it('classifies interrupted response bodies as retryable transport failures', async () => {
    const response = new Response('{}');
    vi.spyOn(response, 'json').mockRejectedValue(new TypeError('Connection terminated while reading body'));
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response));
    await expect(api('/api/links/a1/attempts', 'POST', { key: 'same-key' })).rejects.toMatchObject({ code: 'network' });
  });
  it('shares the exchange promise across duplicate initializations', async () => {
    vi.resetModules();
    const take = vi.fn().mockReturnValue('memory-only-secret');
    vi.stubGlobal('window', { __takeUpfileSecret: take });
    const fetch = vi.fn().mockResolvedValue(new Response('{"id":"a1"}'));
    vi.stubGlobal('fetch', fetch);
    const { openPublicLink } = await import('./api');
    const first = openPublicLink('a1');
    expect(openPublicLink('a1')).toBe(first);
    await first;
    expect(take).toHaveBeenCalledTimes(1);
    expect(fetch).toHaveBeenCalledTimes(1);
  });
});
describe('display and input values', () => {
  it('uses explicit binary units for file-size display', () => {
    expect(bytes(1024)).toBe('1 KiB');
  });
  describe('early fragment capture', () => {
    it('strips the URL synchronously and exposes the secret only once in memory', () => {
      const replaceState = vi.fn();
      const window = {
        location: { hash: '#private-fragment', pathname: '/u/a1', search: '' },
        history: { replaceState },
        matchMedia: () => ({ matches: false, addEventListener: vi.fn() }),
        __takeUpfileSecret: undefined as (() => string) | undefined,
      };
      runInNewContext(readFileSync(new URL('../public/bootstrap.js', import.meta.url), 'utf8'), {
        window, URLSearchParams, document: { documentElement: { setAttribute: vi.fn() } },
      });
      expect(replaceState).toHaveBeenCalledWith(null, '', '/u/a1');
      const take = window.__takeUpfileSecret!;
      expect(take()).toBe('private-fragment');
      expect(window.__takeUpfileSecret).toBeUndefined();
      expect(take()).toBe('');
    });
  });
  it('counts UTF-8 bytes rather than UTF-16 units and converts Unix seconds', () => {
    expect(utf8Length('🙂')).toBe(4);
    const seconds = 1900000000;
    expect(Math.abs(new Date(localDateTime(seconds)).getTime() / 1000 - seconds)).toBeLessThan(60);
  });
});
