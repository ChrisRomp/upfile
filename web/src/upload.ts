import { Upload, type DetailedError, type UploadOptions } from 'tus-js-client';
import { api, APIError, errorMessage, retryable, wait, withRetry } from './api';
import { bytes, utf8Length } from './format';
import type { Attempt, PublicLink } from './types';

export type QueueStatus = 'queued' | 'admitting' | 'uploading' | 'verifying' | 'completed' | 'failed' | 'canceled';
export interface QueueItem {
  id: string;
  key: string;
  file: File;
  comment: string;
  status: QueueStatus;
  sent: number;
  message: string;
  attempt?: Attempt;
  fresh?: boolean;
  submitted?: boolean;
}
interface QueueState { items: QueueItem[]; running: boolean; message: string }
interface Control { canceled: boolean; stop?: () => Promise<void> }
class CancelRequested extends Error {}

export function tusError(error: Error | DetailedError): APIError {
  if (error instanceof APIError) return error;
  if ('causingError' in error && error.causingError instanceof APIError) return error.causingError;
  const response = 'originalResponse' in error ? error.originalResponse : null;
  const status = response?.getStatus() || 0;
  let data: { error?: string; code?: string } = {};
  try { data = JSON.parse(response?.getBody() || '{}'); } catch { /* tus may return plain text. */ }
  const codes: Record<number, string> = {
    401: 'session_required', 403: 'link_unavailable', 404: 'attempt_unavailable',
    410: 'attempt_unavailable', 413: 'too_large', 423: 'busy', 507: 'storage_full',
  };
  const code = data.code || codes[status] || (status === 409 ? 'offset_conflict' : status ? `http_${status}` : 'network');
  const messages: Record<string, string> = {
    session_required: 'Your upload session expired. Reopen the original shared link.',
    link_unavailable: 'This link is expired, revoked, or no longer available.',
    attempt_unavailable: 'This upload is no longer available. Start the file again.',
    too_large: 'This file is larger than the current allowed size.',
    busy: 'Another upload is using this link. Wait, then check its status.',
    storage_full: 'Storage is full. Contact the person who shared this link.',
  };
  return new APIError(data.error || messages[code] || 'The transfer was interrupted. Retry to resume the same upload.', code, status);
}

export function shouldRetryTus(error: Error | DetailedError): boolean {
  const normalized = tusError(error);
  return retryable(normalized) || (normalized.status === 409 && normalized.code === 'offset_conflict');
}

export function validateFile(file: File, comment: string, max: number): string {
  if (file.size > max) return `This file exceeds the ${bytes(max)} per-file limit.`;
  if (utf8Length(file.name) > 255) return 'The filename exceeds 255 UTF-8 bytes. Rename it locally and choose it again.';
  if (utf8Length(comment) > 2048) return 'The comment exceeds 2,048 UTF-8 bytes.';
  return '';
}

export class UploadQueue {
  private state: QueueState = { items: [], running: false, message: '' };
  private listeners = new Set<() => void>();
  private active?: { id: string; control: Control };
  private stopQueue = false;
  constructor(private link: PublicLink) {}
  subscribe = (listener: () => void) => { this.listeners.add(listener); return () => { this.listeners.delete(listener); }; };
  snapshot = () => this.state;
  setLink(link: PublicLink) { this.link = link; }
  private publish(update: Partial<QueueState>) {
    this.state = { ...this.state, ...update };
    this.listeners.forEach((listener) => listener());
  }
  private patch(id: string, patch: Partial<QueueItem>) {
    this.publish({ items: this.state.items.map((item) => item.id === id ? { ...item, ...patch } : item) });
  }
  add(files: File[]) {
    const items = files.map((file): QueueItem => ({
      id: crypto.randomUUID(), key: crypto.randomUUID(), file, comment: '',
      status: 'queued', sent: 0, message: '',
    }));
    this.publish({ items: [...this.state.items, ...items], message: '' });
  }
  comment(id: string, comment: string) {
    const item = this.state.items.find((entry) => entry.id === id);
    if (!this.state.running && item?.status === 'queued' && !item.submitted) this.patch(id, { comment });
  }
  remove(id: string) {
    this.publish({ items: this.state.items.filter((item) => item.id !== id || item.status !== 'queued' || item.submitted) });
  }
  clearFinished() {
    this.publish({ items: this.state.items.filter((item) => item.status !== 'completed' && item.status !== 'canceled') });
  }
  retry(id: string) {
    const item = this.state.items.find((entry) => entry.id === id);
    if (!item || this.state.running) return;
    this.patch(id, {
      status: 'queued', message: '',
      ...(item.fresh || item.status === 'canceled' ? { key: crypto.randomUUID(), attempt: undefined, fresh: false, submitted: false, sent: 0 } : {}),
    });
    this.publish({ message: '' });
  }
  async cancel(id: string): Promise<void> {
    if (this.active?.id === id) {
      this.active.control.canceled = true;
      await this.active.control.stop?.();
    } else {
      const item = this.state.items.find((entry) => entry.id === id);
      if (item?.status === 'queued' && !item.submitted) this.patch(id, { status: 'canceled', message: 'Not sent.' });
      else if (item && item.status !== 'completed' && item.status !== 'canceled') await this.cancelRemote(item);
    }
  }
  async cancelAll(): Promise<void> {
    this.stopQueue = true;
    const active = this.active;
    // Non-active queued files have not been admitted during this run.
    for (const item of this.state.items) {
      if (item.id !== active?.id && item.status === 'queued') await this.cancel(item.id);
    }
    if (active) await this.cancel(active.id);
  }
  async start(): Promise<void> {
    if (this.state.running) return;
    this.stopQueue = false;
    this.publish({ running: true, message: '' });
    try {
      while (!this.stopQueue) {
        const item = this.state.items.find((entry) => entry.status === 'queued');
        if (!item) break;
        const control: Control = { canceled: false };
        this.active = { id: item.id, control };
        try {
          await this.runFile(item, control);
        } catch (error) {
          const current = this.state.items.find((entry) => entry.id === item.id)!;
          if (control.canceled || error instanceof CancelRequested) {
            try { await this.cancelRemote(current); }
            catch (cancelError) {
              this.patch(item.id, { status: 'failed', message: `Cancellation not confirmed: ${errorMessage(cancelError)}` });
              this.stopQueue = true;
              this.publish({ message: 'Cancellation could not be confirmed. Retry cancellation or check the link status.' });
            }
          } else {
            const normalized = error instanceof APIError ? error : new APIError(errorMessage(error), 'transfer_failed');
            this.patch(item.id, { status: 'failed', message: normalized.message });
            const fileSpecific = ['too_large', 'invalid_input', 'idempotency_conflict'].includes(normalized.code);
            if (fileSpecific && !current.attempt) this.patch(item.id, { fresh: true, submitted: false });
            if (normalized.code === 'attempt_unavailable') this.patch(item.id, { fresh: true });
            if (fileSpecific && current.attempt) {
              try {
                const completed = await this.cancelRemote(current, true);
                if (!completed) this.patch(item.id, { status: 'failed', message: normalized.message, fresh: true });
              } catch (cleanupError) {
                this.patch(item.id, { message: `${normalized.message} Cleanup not confirmed: ${errorMessage(cleanupError)}` });
                this.stopQueue = true;
              }
            }
            if (!fileSpecific) this.stopQueue = true;
            if (this.stopQueue) this.publish({ message: `${normalized.message} Remaining files are paused; completed files are safe.` });
          }
        } finally {
          this.active = undefined;
        }
      }
    } finally {
      this.publish({ running: false });
    }
  }
  private async cancelRemote(item: QueueItem, quiet = false): Promise<boolean> {
    if (!item.attempt) {
      // A lost admission response may have created an attempt. Recover it with the original key.
      if (item.submitted) {
        const attempt = await withRetry(() => this.admit(item));
        item = { ...item, attempt };
        this.patch(item.id, { attempt });
      } else {
        this.patch(item.id, { status: 'canceled', message: 'Not sent.' });
        return false;
      }
    }
    const receipt = await withRetry(() => api<Attempt>(this.attemptPath(item.attempt!.id)));
    if (receipt.status === 'completed') {
      this.patch(item.id, { status: 'completed', sent: item.file.size, message: 'Received before cancellation.', attempt: receipt });
      return true;
    }
    if (receipt.status !== 'canceled' && receipt.status !== 'abandoned') {
      await withRetry(() => api(this.attemptPath(item.attempt!.id) + '/cancel', 'POST', {}));
      const settled = await withRetry(() => api<Attempt>(this.attemptPath(item.attempt!.id)));
      if (settled.status === 'completed') {
        this.patch(item.id, { status: 'completed', sent: item.file.size, message: 'Received before cancellation.', attempt: settled });
        return true;
      }
      if (settled.status !== 'canceled' && settled.status !== 'abandoned') {
        throw new APIError('The server has not confirmed that the upload was canceled. Check its status again.', 'cancellation_pending');
      }
    }
    this.patch(item.id, { status: 'canceled', fresh: true, message: quiet ? '' : 'Canceled. The partial upload was discarded.' });
    return false;
  }
  private attemptPath(id: string) { return `/api/links/${this.link.id}/attempts/${id}`; }
  private admit(item: QueueItem) {
    return api<Attempt>(`/api/links/${this.link.id}/attempts`, 'POST', {
      key: item.key, name: item.file.name, comment: item.comment, size: item.file.size,
    });
  }
  private async runFile(item: QueueItem, control: Control) {
    const invalid = validateFile(item.file, item.comment, this.link.max_file_bytes);
    if (invalid) throw new APIError(invalid, 'invalid_input');
    this.patch(item.id, { status: 'admitting', submitted: true, message: 'Preparing upload…' });
    const attempt = item.attempt
      ? await withRetry(() => api<Attempt>(this.attemptPath(item.attempt!.id)))
      : await withRetry(() => this.admit(item), (count) => this.patch(item.id, { message: `Preparing upload · retry ${count}/3…` }));
    item = { ...item, attempt };
    this.patch(item.id, { attempt });
    if (attempt.status === 'completed') {
      this.patch(item.id, { status: 'completed', sent: item.file.size, message: 'Received.' });
      return;
    }
    if (control.canceled) throw new CancelRequested();
    if (attempt.status === 'canceled' || attempt.status === 'abandoned') {
      this.patch(item.id, { fresh: true });
      throw new APIError('This attempt ended. Select Start fresh to send a new upload.', 'attempt_unavailable');
    }
    if (attempt.status === 'uploading') await this.transfer(item, attempt, control);
    this.patch(item.id, { status: 'verifying', message: 'Confirming receipt…' });
    for (let index = 0; index < 8; index++) {
      const receipt = await withRetry(() => api<Attempt>(this.attemptPath(attempt.id)));
      if (receipt.status === 'completed') {
        this.patch(item.id, { status: 'completed', sent: item.file.size, message: 'Received.', attempt: receipt });
        return;
      }
      if (control.canceled) throw new CancelRequested();
      if (receipt.status === 'canceled' || receipt.status === 'abandoned') {
        this.patch(item.id, { fresh: true });
        throw new APIError(`This upload was ${receipt.status}. It was not received.`, 'attempt_unavailable');
      }
      await wait(Math.min(500 * (index + 1), 2500));
    }
    throw new APIError('Receipt is not yet confirmed. Retry to check the same upload; do not submit it again.', 'receipt_pending');
  }
  private transfer(item: QueueItem, attempt: Attempt, control: Control): Promise<void> {
    const url = new URL(attempt.upload_url, window.location.origin);
    const expected = `/api/links/${this.link.id}/uploads/${attempt.id}`;
    if (url.origin !== window.location.origin || url.pathname !== expected || url.search || url.hash || url.username || url.password) {
      throw new APIError('The server returned an invalid upload address. Contact the administrator.', 'invalid_response');
    }
    if (!Number.isSafeInteger(this.link.chunk_bytes) || this.link.chunk_bytes <= 0) {
      throw new APIError('The server returned an invalid upload chunk size.', 'invalid_response');
    }
    this.patch(item.id, { status: 'uploading', sent: attempt.offset, message: 'Uploading…' });
    return new Promise<void>((resolve, reject) => {
      let retryCount = 0;
      const options: UploadOptions & { withCredentials: true } = {
        uploadUrl: url.href,
        chunkSize: this.link.chunk_bytes,
        withCredentials: true,
        headers: { 'X-Upfile-Request': '1' },
        storeFingerprintForResuming: false,
        retryDelays: [600, 1500, 3500],
        onBeforeRequest: (request) => {
          if (control.canceled) throw new CancelRequested();
          if (!['HEAD', 'PATCH'].includes(request.getMethod()) || request.getURL() !== url.href) {
            throw new APIError('An unsafe upload request was blocked.', 'invalid_response');
          }
          const xhr = request.getUnderlyingObject() as XMLHttpRequest;
          xhr.withCredentials = true;
          xhr.timeout = 60_000;
        },
        onShouldRetry: (error) => {
          if (control.canceled || retryCount >= 5 || !shouldRetryTus(error)) return false;
          retryCount++;
          this.patch(item.id, { message: `Connection interrupted · retry ${retryCount}/5…` });
          return true;
        },
        onProgress: (sent) => this.patch(item.id, { sent: Math.min(sent, item.file.size) }),
        onChunkComplete: () => this.patch(item.id, { message: 'Uploading…' }),
        onError: (error) => reject(tusError(error)),
        onSuccess: () => resolve(),
      };
      const upload = new Upload(item.file, options);
      control.stop = async () => {
        // Never pass true: tus DELETE is not an application cancellation.
        try {
          await upload.abort(false);
          reject(new CancelRequested());
        } catch {
          control.canceled = false;
          const error = new APIError('The browser could not stop the transfer. Keep this page open and retry cancellation.', 'abort_failed');
          reject(error);
          throw error;
        }
      };
      upload.start();
    }).finally(() => { control.stop = undefined; });
  }
}
