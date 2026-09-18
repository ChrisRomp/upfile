import type { PublicLink } from './types';

export class APIError extends Error {
  constructor(message: string, public code: string, public status = 0) {
    super(message);
    this.name = 'APIError';
  }
}

export const errorMessage = (error: unknown): string =>
  error instanceof Error ? error.message : 'Something went wrong. Please try again.';

export async function api<T>(path: string, method = 'GET', body?: unknown): Promise<T> {
  let response: Response;
  try {
    response = await fetch(path, {
      method,
      credentials: 'same-origin',
      cache: 'no-store',
      headers: method === 'GET' ? { Accept: 'application/json' } : {
        Accept: 'application/json',
        'Content-Type': 'application/json',
        'X-Upfile-Request': '1',
      },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: AbortSignal.timeout(30_000),
    });
  } catch {
    throw new APIError('The server could not be reached. Check your connection and retry.', 'network');
  }
  const data = await response.json().catch((error: unknown) => {
    if (error instanceof SyntaxError) return null;
    throw new APIError('The response was interrupted. Check your connection and retry.', 'network');
  });
  if (!response.ok) {
    throw new APIError(
      typeof data?.error === 'string' ? data.error :
        response.status === 401 || response.status === 403
          ? 'Access was denied or your session expired. Reopen the original link or sign in again.'
          : `The request failed (${response.status}). Please retry.`,
      typeof data?.code === 'string' ? data.code : `http_${response.status}`,
      response.status,
    );
  }
  if (data === null) {
    throw new APIError('The server returned an unexpected response. Reload or sign in again.', 'invalid_response');
  }
  return data as T;
}

const permanent = new Set([
  'session_required', 'link_unavailable', 'busy', 'storage_full', 'record_limit',
  'too_large', 'invalid_input', 'idempotency_conflict', 'attempt_unavailable',
]);
export function retryable(error: unknown): boolean {
  return error instanceof APIError && !permanent.has(error.code) &&
    (error.code === 'network' || error.status === 408 || error.status === 429 || error.status >= 500);
}

export const wait = (milliseconds: number) => new Promise<void>((resolve) => setTimeout(resolve, milliseconds));
export async function withRetry<T>(action: () => Promise<T>, onRetry?: (attempt: number) => void): Promise<T> {
  for (let attempt = 0; ; attempt++) {
    try {
      return await action();
    } catch (error) {
      if (attempt >= 3 || !retryable(error)) throw error;
      onRetry?.(attempt + 1);
      await wait([600, 1500, 3500][attempt]);
    }
  }
}

declare global {
  interface Window { __takeUpfileSecret?: () => string }
}

const initialSessions = new Map<string, Promise<PublicLink>>();
export function openPublicLink(id: string): Promise<PublicLink> {
  const existing = initialSessions.get(id);
  if (existing) return existing;
  let secret = window.__takeUpfileSecret?.() || '';
  // The promise is shared across StrictMode mounts, including failed exchanges.
  const result = secret
    ? withRetry(() => api<PublicLink>(`/api/links/${id}/exchange`, 'POST', { secret }))
      .finally(() => { secret = ''; })
    : api<PublicLink>(`/api/links/${id}`);
  initialSessions.set(id, result);
  return result;
}
