import { useEffect, useRef, useState, useSyncExternalStore } from 'react';
import { api, APIError, errorMessage, openPublicLink } from './api';
import { bytes, date, utf8Length } from './format';
import type { PublicLink } from './types';
import { UploadQueue, validateFile } from './upload';
import { Badge, Brand, Confirm, Empty, Footer, Notice } from './ui';

export default function Public({ id }: { id: string }) {
  const [link, setLink] = useState<PublicLink>();
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(true);
  useEffect(() => {
    let mounted = true;
    openPublicLink(id).then((info) => {
      if (mounted) setLink(info);
    }).catch((error) => {
      if (mounted) setError(publicError(error));
    }).finally(() => { if (mounted) setLoading(false); });
    return () => { mounted = false; };
  }, [id]);
  async function retry() {
    setLoading(true); setError('');
    try { setLink(await api<PublicLink>(`/api/links/${id}`)); }
    catch (error) { setError(publicError(error)); }
    finally { setLoading(false); }
  }
  return <><Brand><span className="muted">File delivery</span></Brand>
    <main className="public-main">
      {loading && <Empty>Opening your upload link…</Empty>}
      <Notice error>{error}</Notice>
      {!loading && !link && <section className="card"><h1>Let’s check your link</h1>
        <p>Open the original shared link to start or restore an upload session. The address in your browser no longer contains the private part of the link.</p>
        <button onClick={retry}>Check session again</button>
      </section>}
      {link && <Uploader initialLink={link} />}
      <Footer>Keep this page open while files are sending or comments are saving. Reloading clears your selected files; it does not restore an unfinished transfer or unsaved comments.</Footer>
    </main>
  </>;
}

function publicError(error: unknown) {
  if (error instanceof APIError && error.code === 'session_required') return 'Your upload session is missing or expired. Reopen the original shared link from the person who sent it.';
  if (error instanceof APIError && error.code === 'link_unavailable') return 'This link is expired, revoked, or no longer available. Ask the sender for a new link.';
  return errorMessage(error);
}

function Uploader({ initialLink }: { initialLink: PublicLink }) {
  const [link, setLink] = useState(initialLink);
  const [queue] = useState(() => new UploadQueue(initialLink));
  const state = useSyncExternalStore(queue.subscribe, queue.snapshot);
  const [error, setError] = useState('');
  const [checking, setChecking] = useState(false);
  const [confirmReset, setConfirmReset] = useState(false);
  const [dragging, setDragging] = useState(false);
  const [now, setNow] = useState(Date.now() / 1000);
  const sending = useRef(false);
  const pendingSend = useRef(false);
  const expired = now >= link.expires_at || now >= link.session_expires_at;
  const hasResumable = state.items.some((item) => item.submitted && !item.fresh && ['failed', 'queued'].includes(item.status));
  const selectionDisabled = expired || (link.busy && !state.running && !hasResumable);
  const disabled = state.running || checking || selectionDisabled;
  const unsavedComments = state.items.some((item) => item.status !== 'canceled' && item.commentStatus !== 'saved');
  const total = state.items.reduce((sum, item) => sum + item.file.size, 0);
  const sent = state.items.reduce((sum, item) => sum + item.sent, 0);
  const completed = state.items.filter((item) => item.status === 'completed').length;
  const progress = total ? sent : state.items.length > 0 && completed === state.items.length ? 1 : 0;
  const failed = state.items.filter((item) => item.status === 'failed').length;
  const canceled = state.items.filter((item) => item.status === 'canceled').length;
  const queued = state.items.filter((item) => item.status === 'queued');
  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now() / 1000), 1000);
    return () => window.clearInterval(timer);
  }, []);
  useEffect(() => {
    const preventClose = (event: BeforeUnloadEvent) => {
      if (state.running || unsavedComments) { event.preventDefault(); event.returnValue = ''; }
    };
    window.addEventListener('beforeunload', preventClose);
    return () => window.removeEventListener('beforeunload', preventClose);
  }, [state.running, unsavedComments]);
  async function check() {
    setChecking(true); setError('');
    try {
      const info = await api<PublicLink>(`/api/links/${link.id}`);
      setLink(info); queue.setLink(info);
      return info;
    } catch (error) { setError(publicError(error)); return undefined; }
    finally { setChecking(false); }
  }
  async function send() {
    if (sending.current) { pendingSend.current = true; return; }
    sending.current = true;
    pendingSend.current = false;
    try {
      const info = await check();
      const resumable = queue.snapshot().items.some((item) => item.submitted && !item.fresh && ['failed', 'queued'].includes(item.status));
      if (!info || (info.busy && !resumable)) return;
      await queue.start();
      await check();
    } finally {
      sending.current = false;
      if (pendingSend.current && !queue.snapshot().message && queue.snapshot().items.some((item) => item.status === 'queued')) void send();
    }
  }
  function add(files: File[]) {
    if (!files.length || selectionDisabled) return;
    queue.add(files);
    void send();
  }
  async function cancel(id?: string) {
    setError('');
    try { if (id) await queue.cancel(id); else await queue.cancelAll(); }
    catch (error) { setError(`Cancellation not confirmed: ${publicError(error)}`); }
  }
  return <>
    <section className="request-heading"><p className="eyebrow">File request</p><h1>{link.title}</h1>
      {link.instructions && <p className="instructions">{link.instructions}</p>}
      <div className="metadata"><span>Up to <strong>{bytes(link.max_file_bytes)}</strong> per file</span><span>Link expires {date(link.expires_at)}</span></div>
      <p className="small muted">You can send multiple files and use this link again until it expires. Session valid until {date(link.session_expires_at)}.</p>
    </section>
    <Notice error>{error}</Notice>
    {expired && <Notice error>This link or upload session has expired. Reopen the original shared link; ask its sender for a new link if it has expired.</Notice>}
    {(link.busy || link.reset_available) && !state.running && <Notice>
      <strong>{link.reset_available ? link.busy ? 'An unfinished upload is blocking this link.' : 'An abandoned upload is available to clean up.' : 'This link has an active upload.'}</strong>
      <p>{link.reset_available ? link.busy ? 'You can discard the stale partial upload and start again. Completed files are kept.' : 'You can send new files or discard the abandoned partial upload to free its reserved space. Completed files are kept.' : 'Wait for the other transfer to finish. If it was interrupted, a reset becomes available after its activity lease expires.'}</p>
      {hasResumable && <p>You have an unfinished attempt on this page. Retry the same upload to recover its receipt or resume it before resetting.</p>}
      <div className="actions"><button onClick={check} disabled={checking}>Check status</button>
        {link.reset_available && <button onClick={() => setConfirmReset(true)}>Reset unfinished upload</button>}</div>
    </Notice>}
    <section className="card queue-card" aria-labelledby="queue-title">
      <div className="section-heading"><h2 id="queue-title">Your files</h2><span className="muted">{state.items.length} selected</span></div>
      <div className={`drop-area ${dragging ? 'dragging' : ''}`}
        onDragOver={(event) => { event.preventDefault(); if (!selectionDisabled) setDragging(true); }}
        onDragLeave={() => setDragging(false)}
        onDrop={(event) => {
          event.preventDefault(); setDragging(false);
          add(Array.from(event.dataTransfer.files));
        }}>
        <span className="upload-symbol" aria-hidden="true">↥</span>
        <label htmlFor="files" className="file-label">Choose files <span className="muted">or drop them here</span></label>
        <input id="files" type="file" multiple disabled={selectionDisabled}
          onChange={(event) => { add(Array.from(event.target.files || [])); event.target.value = ''; }} />
        <p className="small muted">Uploads start automatically, one file at a time. Add comments while files send; changes save automatically.</p>
      </div>
      {state.items.length === 0 && <Empty>No files selected yet.</Empty>}
      <ul className="queue-list">
        {state.items.map((item) => {
          const active = ['admitting', 'uploading', 'verifying'].includes(item.status);
          const issue = item.status === 'queued' ? validateFile(item.file, '', link.max_file_bytes) : '';
          return <li key={item.id} className="queue-item">
            <div className="section-heading"><div className="file-heading"><strong>{item.file.name}</strong><span className="small muted">{bytes(item.file.size)}</span></div><Badge status={item.status} /></div>
            <label className="comment-label" htmlFor={`comment-${item.id}`}>
              Comment <span className="muted">(optional · {utf8Length(item.comment).toLocaleString()}/2,048 bytes)</span>
              <textarea id={`comment-${item.id}`} rows={2} value={item.comment}
                disabled={expired || item.status === 'canceled'}
                aria-invalid={utf8Length(item.comment) > 2048}
                onChange={(event) => queue.comment(item.id, event.target.value)} />
            </label>
            {item.status !== 'canceled' && <div className="small">
              <span className={item.commentError ? 'error-text' : 'muted'} role={item.commentError ? 'alert' : 'status'}>
                {item.commentError || (item.commentStatus === 'saved' ? 'Comment saved.' :
                  item.commentStatus === 'saving' ? 'Saving comment…' : 'Comment not yet saved…')}
              </span>
              {item.commentStatus === 'failed' && utf8Length(item.comment) <= 2048 &&
                <button disabled={expired} onClick={() => queue.retryComment(item.id)}>Retry comment</button>}
            </div>}
            {(active || item.sent > 0) && <><progress aria-label={`Upload progress for ${item.file.name}`} value={item.sent} max={item.file.size || 1} />
              <span className="small muted">{bytes(item.sent)} / {bytes(item.file.size)}</span></>}
            <div className="item-footer"><span className={issue || item.status === 'failed' ? 'error-text' : 'muted'} role={item.status === 'failed' ? 'alert' : 'status'}>{issue || item.message}</span>
              <div className="actions">
                {item.status === 'queued' && !state.running && !item.submitted && <button onClick={() => queue.remove(item.id)}>Remove</button>}
                {item.status === 'queued' && !state.running && item.submitted && <button onClick={() => cancel(item.id)}>Cancel attempt</button>}
                {(active || (item.status === 'queued' && state.running)) && <button onClick={() => cancel(item.id)}>Cancel</button>}
                {item.status === 'failed' && !state.running && item.submitted && !item.fresh && <button onClick={() => cancel(item.id)}>Cancel attempt</button>}
                {(item.status === 'failed' || item.status === 'canceled') && <button disabled={disabled} onClick={() => { queue.retry(item.id); void send(); }}>
                  {item.fresh || item.status === 'canceled' ? 'Start fresh' : 'Retry same upload'}
                </button>}
              </div>
            </div>
          </li>;
        })}
      </ul>
      {!!state.items.length && <div className="queue-summary">
        <div className="section-heading"><strong>Overall progress</strong><span>{Math.floor(progress / (total || 1) * 100)}%</span></div>
        <progress aria-label="Overall byte progress" max={total || 1} value={progress} />
        <div className="summary-counts" aria-live="polite" aria-atomic="true">{completed} received · {failed} failed · {canceled} canceled · {queued.length} waiting</div>
        <p className="small muted">{bytes(sent)} / {bytes(total)} transferred. Only “completed” means the server confirmed receipt.</p>
      </div>}
      <Notice error>{state.message}</Notice>
      <div className="actions send-actions">
        {!state.running && (completed > 0 || canceled > 0) && <button disabled={unsavedComments} onClick={() => queue.clearFinished()}>Clear finished</button>}
        {state.running ? <button className="danger" onClick={() => cancel()}>Cancel remaining</button> :
          queued.length > 0 && <button className="primary" disabled={disabled} onClick={send}>{checking ? 'Checking…' : 'Resume uploads'}</button>}
      </div>
    </section>
    {confirmReset && <Confirm title="Discard the unfinished upload?" label="Reset upload" onClose={() => setConfirmReset(false)} action={async () => {
      await api(`/api/links/${link.id}/reset`, 'POST', {});
      setConfirmReset(false);
      await check();
    }}><p>This discards the stale partial file. It does not expose another session’s files or remove completed deliveries. You will need to select and send the unfinished file again.</p></Confirm>}
  </>;
}
