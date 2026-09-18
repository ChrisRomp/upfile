import { useEffect, useState } from 'react';
import { api, errorMessage } from './api';
import { bytes, date, localDateTime, megabytes, readMegabytes, utf8Length } from './format';
import type { Audit, Collection, Container, CreatedContainer, Link, ReceivedFile, Settings } from './types';
import { Badge, Brand, Confirm, Empty, Modal, MutationForm, Notice, Pagination, Search } from './ui';

const LIMIT = 25;
function useResource<T>(path: string) {
  const [data, setData] = useState<T>();
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(true);
  const [revision, setRevision] = useState(0);
  useEffect(() => {
    let current = true;
    setLoading(true); setError('');
    api<T>(path).then((value) => { if (current) setData(value); })
      .catch((error) => { if (current) setError(errorMessage(error)); })
      .finally(() => { if (current) setLoading(false); });
    return () => { current = false; };
  }, [path, revision]);
  return { data, error, loading, refresh: () => setRevision((value) => value + 1) };
}
function useCollection<T>(path: string) {
  const [query, setQuery] = useState('');
  const [debounced, setDebounced] = useState('');
  const [page, setPage] = useState(1);
  useEffect(() => {
    const timer = window.setTimeout(() => { setDebounced(query); setPage(1); }, 250);
    return () => clearTimeout(timer);
  }, [query]);
  const resource = useResource<Collection<T>>(`${path}?${new URLSearchParams({ q: debounced, page: String(page), limit: String(LIMIT) })}`);
  return { ...resource, query, setQuery, page, setPage };
}
function FetchStatus({ error, loading, retry }: { error: string; loading: boolean; retry: () => void }) {
  return <><Notice error>{error && <>{error} <button onClick={retry}>Retry</button></>}</Notice>
    {loading && <p className="small muted" role="status">Loading…</p>}</>;
}

export default function Admin() {
  const settings = useResource<Settings>('/api/settings');
  const path = window.location.pathname.replace(/\/$/, '') || '/';
  const containerID = /^\/containers\/([a-f0-9]+)$/.exec(path)?.[1];
  const firstRun = settings.data && !settings.data.configured;
  const tab = firstRun || path === '/settings' ? 'settings' : path === '/audit' ? 'audit' : 'containers';
  return <><Brand><nav className="main-nav" aria-label="Main navigation">
    <a href="/" aria-current={tab === 'containers' ? 'page' : undefined}>Requests</a>
    <a href="/settings" aria-current={tab === 'settings' ? 'page' : undefined}>Settings</a>
    <a href="/audit" aria-current={tab === 'audit' ? 'page' : undefined}>Activity</a>
  </nav><span className="admin-label">Admin</span></Brand>
    <main className="admin-main">
      <FetchStatus {...settings} retry={settings.refresh} />
      {settings.data && <>
        {firstRun || path === '/settings' ? <SettingsPage key={String(settings.data.configured)} settings={settings.data} refresh={settings.refresh} /> :
          path === '/audit' ? <AuditPage /> :
            containerID ? <ContainerPage id={containerID} settings={settings.data} refreshSettings={settings.refresh} /> :
              path === '/' || path === '/containers' ? <ContainersPage settings={settings.data} /> :
                <Empty><h1>Page not found</h1><a href="/">Back to requests</a></Empty>}
      </>}
      <footer>Files are untrusted and are not malware-scanned. Download carefully. No file previews.</footer>
    </main>
  </>;
}

function SizeField({ label, value, onChange, inherited }: {
  label: string; value: string; onChange: (value: string) => void; inherited?: number;
}) {
  const inheritedLabel = inherited === undefined ? undefined : `${megabytes(inherited)} MB`;
  const inheritanceHint = inheritedLabel === undefined ? '' : `Leave blank to inherit ${inheritedLabel}. An override can only lower the parent limit.`;
  return <label>{label} <span className="muted">(MB{inherited !== undefined ? ', optional' : ''})</span>
    <input type="number" min="0.000001" step="any"
      max={megabytes(inherited ?? Number.MAX_SAFE_INTEGER)} value={value}
      required={inherited === undefined} onChange={(event) => onChange(event.target.value)}
      placeholder={inheritedLabel !== undefined ? `Inherit ${inheritedLabel}` : undefined} />
    <span className="field-hint">{`1 MB = 1,000,000 bytes. Decimals are allowed. ${inheritanceHint}`.trim()}</span>
  </label>;
}

function SettingsPage({ settings, refresh }: { settings: Settings; refresh: () => void }) {
  const [max, setMax] = useState(settings.configured ? megabytes(settings.max_file_bytes) : '');
  const [budget, setBudget] = useState(settings.configured ? megabytes(settings.storage_budget_bytes) : '');
  const [hours, setHours] = useState(String(settings.default_link_hours || 168));
  const [message, setMessage] = useState('');
  const cleanup = Array.isArray(settings.cleanup_errors) ? settings.cleanup_errors.length : settings.cleanup_errors;
  const used = settings.stored_bytes + settings.reserved_bytes;
  return <>
    <div className="page-heading"><div><p className="eyebrow">{settings.configured ? 'Administration' : 'Before your first request'}</p><h1>{settings.configured ? 'Settings' : 'Welcome to upfile'}</h1>
      <p className="muted">{settings.configured ? 'Storage limits and defaults for new links.' : 'Choose a file limit and storage budget to begin accepting files.'}</p></div>
      <button onClick={refresh}>Refresh usage</button></div>
    <div className="settings-grid">
      <section className="card"><h2>{settings.configured ? 'Upload defaults' : 'Set up your storage'}</h2>
        <Notice>{message}</Notice>
        <MutationForm label={settings.configured ? 'Save settings' : 'Save and get started'} submit={async () => {
          setMessage('');
          const defaultHours = Number(hours);
          if (!Number.isSafeInteger(defaultHours) || defaultHours <= 0) throw new Error('Link lifetime must be a positive whole number of hours.');
          await api('/api/settings', 'PUT', { max_file_bytes: readMegabytes(max), storage_budget_bytes: readMegabytes(budget), default_link_hours: defaultHours });
          setMessage('Settings saved. New limits also apply to unfinished uploads.'); refresh();
        }}>
          <SizeField label="Global maximum file size" value={max} onChange={setMax} />
          <SizeField label="Total storage budget" value={budget} onChange={setBudget} />
          <label>Default link lifetime <span className="muted">(hours)</span><input type="number" min="1" step="1" required value={hours} onChange={(event) => setHours(event.target.value)} />
            <span className="field-hint">168 hours = 7 days. Changes only affect new links.</span></label>
          <p className="small muted">Lower limits do not delete received files. A budget below current usage blocks new uploads. Container and link limits may tighten, but never raise, the global maximum.</p>
        </MutationForm>
      </section>
      <aside className="card usage-card"><h2>Storage</h2>
        <strong className="usage-number">{bytes(used)}</strong><p className="muted">of {bytes(settings.storage_budget_bytes)} allocated</p>
        <progress aria-label="Allocated storage" value={Math.min(used, settings.storage_budget_bytes)} max={settings.storage_budget_bytes || 1} />
        <dl><div><dt>Received files</dt><dd>{bytes(settings.stored_bytes)}</dd></div>
          <div><dt>Reserved for uploads</dt><dd>{bytes(settings.reserved_bytes)}</dd></div>
          <div><dt>Records</dt><dd>{settings.record_count.toLocaleString()} / {settings.max_records.toLocaleString()}</dd></div>
          <div><dt>Chunk ceiling</dt><dd>{bytes(settings.chunk_bytes)}</dd></div>
          <div><dt>Activity lease</dt><dd>{settings.lease_seconds} seconds</dd></div></dl>
        {used >= settings.storage_budget_bytes && settings.configured && <Notice error>Storage budget reached. New uploads are blocked until space is freed or the budget is raised.</Notice>}
        {!!cleanup && <Notice error><strong>{cleanup} cleanup {cleanup === 1 ? 'error' : 'errors'} outstanding</strong>
          <p>Deletion may still be pending. Check service logs and storage health.</p>
          {Array.isArray(settings.cleanup_errors) && <ul>{settings.cleanup_errors.map((error, index) => <li key={index}>{error}</li>)}</ul>}</Notice>}
      </aside>
    </div>
  </>;
}

function ContainerForm({ initial, globalMax, onSaved, onClose }: {
  initial?: Container; globalMax: number; onSaved: (url?: string) => void; onClose: () => void;
}) {
  const [name, setName] = useState(initial?.name || '');
  const [instructions, setInstructions] = useState(initial?.instructions || '');
  const [max, setMax] = useState(initial?.max_file_bytes == null ? '' : megabytes(initial.max_file_bytes));
  return <Modal title={initial ? 'Edit request' : 'New request'} onClose={onClose}>
    <MutationForm label={initial ? 'Save request' : 'Create request'} onCancel={onClose} submit={async () => {
      const cap = readMegabytes(max, true);
      if (cap !== null && cap > globalMax) throw new Error('The request limit cannot exceed the global maximum.');
      const body = { name: name.trim(), instructions, max_file_bytes: cap };
      if (initial) {
        await api<Container>(`/api/containers/${initial.id}`, 'PUT', body);
        onClose(); onSaved();
      } else {
        const result = await api<CreatedContainer>('/api/containers', 'POST', body);
        const url = result.initial_link?.url;
        if (!url) throw new Error('The request was created, but its upload URL was missing. Open the request and replace its link to obtain a new URL.');
        onClose(); onSaved(url);
      }
    }}>
      <label>Request name<input autoFocus required maxLength={255} value={name} onChange={(event) => setName(event.target.value)} /></label>
      <label>Public instructions <span className="muted">(optional)</span><textarea rows={4} value={instructions} onChange={(event) => setInstructions(event.target.value)} />
        <span className="field-hint">Visible to anyone with an upload link. Plain text only.</span></label>
      <SizeField label="Maximum file size" value={max} onChange={setMax} inherited={globalMax} />
      {!initial && <p className="small muted">An upload link labeled “Default link” is created automatically, with the default expiration and this request’s file-size limit. You can copy it next and edit its settings later.</p>}
    </MutationForm>
  </Modal>;
}
function ContainersPage({ settings }: { settings: Settings }) {
  const collection = useCollection<Container>('/api/containers');
  const [create, setCreate] = useState(false);
  const [createdURL, setCreatedURL] = useState<string>();
  return <>
    <div className="page-heading"><div><p className="eyebrow">Your workspace</p><h1>File requests</h1><p className="muted">A place for each request. A private link for each sender.</p></div>
      <button className="primary" onClick={() => setCreate(true)}>＋ New request</button></div>
    <div className="usage-strip"><span><strong>{bytes(settings.stored_bytes)}</strong> received</span><span>{bytes(settings.reserved_bytes)} reserved</span><a href="/settings">Manage storage →</a></div>
    <section className="card">
      <div className="toolbar"><Search placeholder="Search requests" value={collection.query} onChange={collection.setQuery} /><button onClick={collection.refresh}>Refresh</button></div>
      <FetchStatus {...collection} retry={collection.refresh} />
      {!collection.loading && !collection.error && !collection.data?.items?.length && <Empty>{collection.query ? 'No requests match your search.' : 'Create a request to get its upload link and start receiving files.'}</Empty>}
      {!!collection.data?.items?.length && <div className="table-scroll"><table><thead><tr><th>Request</th><th>Files</th><th>Storage</th><th>Links</th><th>Recent activity</th><th>Status</th></tr></thead>
        <tbody>{collection.data.items.map((item) => <tr key={item.id}>
          <td><a className="strong-link" href={`/containers/${item.id}`}>{item.name}</a><span className="table-subtext">{item.active_uploads} uploading · {item.active_downloads} downloading</span></td>
          <td>{item.file_count}</td><td>{bytes(item.stored_bytes)}</td><td>{item.link_count}</td><td className="nowrap">{date(item.last_activity ?? item.created_at)}</td><td><Badge status={item.status} /></td>
        </tr>)}</tbody></table></div>}
      {collection.data && <Pagination page={collection.page} total={collection.data.total} limit={LIMIT} onChange={collection.setPage} />}
    </section>
    {create && <ContainerForm globalMax={settings.max_file_bytes} onClose={() => setCreate(false)} onSaved={(url) => {
      collection.refresh();
      if (url) setCreatedURL(url);
    }} />}
    {createdURL && <SecretLink url={createdURL} onClose={() => setCreatedURL(undefined)} />}
  </>;
}

function ContainerPage({ id, settings, refreshSettings }: { id: string; settings: Settings; refreshSettings: () => void }) {
  const resource = useResource<Container>(`/api/containers/${id}`);
  const [tab, setTab] = useState<'files' | 'links'>('files');
  const [edit, setEdit] = useState(false);
  const [deleting, setDeleting] = useState<Container>();
  const [actionError, setActionError] = useState('');
  const [message, setMessage] = useState('');
  const [deleted, setDeleted] = useState(false);
  const [refreshVersion, setRefreshVersion] = useState(0);
  const container = resource.data;
  function refresh() { resource.refresh(); refreshSettings(); setRefreshVersion((value) => value + 1); }
  async function confirmDeletion() {
    setActionError('');
    try { setDeleting(await api<Container>(`/api/containers/${id}`)); }
    catch (error) { setActionError(errorMessage(error)); }
  }
  return <>
    <a className="back-link" href="/">← All requests</a>
    <FetchStatus {...resource} retry={resource.refresh} /><Notice error>{actionError}</Notice><Notice>{message}</Notice>
    {deleted ? <section className="card"><h1>Request deleted</h1><p>The request and its contents have been removed.</p><a href="/">Back to requests</a></section> : container && <>
      <div className="page-heading"><div><p className="eyebrow">File request</p><h1>{container.name}</h1>
        <div className="metadata"><Badge status={container.status} /><span>{container.file_count} files · {bytes(container.stored_bytes)}</span><span>{container.active_uploads} uploading · {container.active_downloads} downloading</span></div></div>
        <div className="actions"><button onClick={refresh}>Refresh</button><button disabled={container.status === 'deleting'} onClick={() => setEdit(true)}>Edit request</button><button className="danger" disabled={container.status === 'deleting'} onClick={confirmDeletion}>Delete request</button></div></div>
      {container.status === 'deleting' && <Notice>Deletion is in progress. Links are disabled; file cleanup may still be running. Refresh to check its status and review Settings for cleanup errors.</Notice>}
      {container.instructions && <p className="instructions">{container.instructions}</p>}
      <p className="small muted">Effective per-file limit: {bytes(container.effective_max_bytes ?? Math.min(settings.max_file_bytes, container.max_file_bytes ?? settings.max_file_bytes))} · {container.max_file_bytes === null ? 'Inherits the global maximum' : `Request override: ${bytes(container.max_file_bytes)}`}</p>
      <div className="tabs" role="tablist" aria-label="Request sections">
        <button id="files-tab" role="tab" aria-selected={tab === 'files'} aria-controls="files-panel" onClick={() => setTab('files')}>Received files</button>
        <button id="links-tab" role="tab" aria-selected={tab === 'links'} aria-controls="links-panel" onClick={() => setTab('links')}>Upload links</button>
      </div>
      <section id={`${tab}-panel`} role="tabpanel" aria-labelledby={`${tab}-tab`}>
        {tab === 'files' ? <FilesPanel key={`files-${refreshVersion}`} container={container} onChange={() => { resource.refresh(); refreshSettings(); }} /> :
          <LinksPanel key={`links-${refreshVersion}`} container={container} settings={settings} onChange={() => { resource.refresh(); refreshSettings(); }} />}
      </section>
      {edit && <ContainerForm initial={container} globalMax={settings.max_file_bytes} onSaved={refresh} onClose={() => setEdit(false)} />}
      {deleting && <Confirm title="Delete this request?" requiredName={deleting.name} label="Delete request and contents" onClose={() => setDeleting(undefined)} action={async () => {
        const result = await api<{ status: string }>(`/api/containers/${id}`, 'DELETE');
        setDeleting(undefined);
        if (result.status === 'deleted') setDeleted(true);
        else { setMessage('Deletion started. The request is closed, but cleanup is still pending.'); resource.refresh(); }
        refreshSettings();
      }}>
        <p>This permanently deletes <strong>{deleting.name}</strong> and its contents. There is no trash.</p>
        <ul className="confirmation-counts"><li><strong>{deleting.file_count}</strong> received files ({bytes(deleting.stored_bytes)})</li>
          <li><strong>{deleting.link_count}</strong> upload links revoked</li><li><strong>{deleting.active_uploads}</strong> active uploads stopped</li>
          <li><strong>{deleting.active_downloads}</strong> active downloads affected</li></ul>
        <p className="small muted">Counts reflect the latest check and may change. Bytes already downloaded cannot be recalled. Cleanup may finish later.</p>
      </Confirm>}
    </>}
  </>;
}

function LinkForm({ initial, container, settings, onSaved, onClose }: {
  initial?: Link; container: Container; settings: Settings; onSaved: (result: Link & { url?: string }) => void; onClose: () => void;
}) {
  const [sender, setSender] = useState(initial?.sender_label || '');
  const [expiration, setExpiration] = useState(localDateTime(initial?.expires_at ?? Math.floor(Date.now() / 1000) + settings.default_link_hours * 3600));
  const [max, setMax] = useState(initial?.max_file_bytes == null ? '' : megabytes(initial.max_file_bytes));
  const parent = container.effective_max_bytes ?? Math.min(settings.max_file_bytes, container.max_file_bytes ?? settings.max_file_bytes);
  return <Modal title={initial ? 'Edit upload link' : 'Create an upload link'} onClose={onClose}>
    <MutationForm label={initial ? 'Save link' : 'Create link'} onCancel={onClose} submit={async () => {
      const expires = Math.floor(new Date(expiration).getTime() / 1000);
      if (!Number.isSafeInteger(expires) || expires <= Date.now() / 1000) throw new Error('Choose an expiration in the future.');
      const cap = readMegabytes(max, true);
      if (cap !== null && cap > parent) throw new Error('The link limit cannot exceed its request’s effective maximum.');
      const result = await api<Link & { url?: string }>(initial ? `/api/links/${initial.id}` : `/api/containers/${container.id}/links`, initial ? 'PUT' : 'POST',
        { sender_label: sender.trim(), expires_at: expires, max_file_bytes: cap });
      onClose(); onSaved(result);
    }}>
      <label>Sender label<input autoFocus required maxLength={255} value={sender} onChange={(event) => setSender(event.target.value)} />
        <span className="field-hint">For your records only. This label does not verify the sender’s identity.</span></label>
      <label>Expires at <span className="muted">(your local time)</span><input required type="datetime-local" value={expiration} onChange={(event) => setExpiration(event.target.value)} /></label>
      <SizeField label="Maximum file size" value={max} onChange={setMax} inherited={parent} />
      <p className="small muted">The link is reusable for multiple files until it expires or you revoke it. Anyone with the full link can upload.</p>
    </MutationForm>
  </Modal>;
}
function SecretLink({ url, onClose }: { url: string; onClose: () => void }) {
  const [message, setMessage] = useState('');
  const [error, setError] = useState('');
  return <Modal title="Your upload link is ready" onClose={onClose}>
    <p>Copy this link now. Its private part will not be shown again. Share it only with the intended sender.</p>
    <label>Private upload link<input className="secret-url" readOnly value={url} onFocus={(event) => event.target.select()} /></label>
    <Notice>{message}</Notice><Notice error>{error}</Notice>
    <div className="actions form-actions"><button onClick={onClose}>Done</button><button className="primary" onClick={async () => {
      setError(''); setMessage('');
      try { await navigator.clipboard.writeText(url); setMessage('Link copied.'); }
      catch { setError('Clipboard access failed. Select the link above and copy it manually.'); }
    }}>Copy link</button></div>
  </Modal>;
}
function LinksPanel({ container, settings, onChange }: { container: Container; settings: Settings; onChange: () => void }) {
  const collection = useCollection<Link>(`/api/containers/${container.id}/links`);
  const [form, setForm] = useState<Link | 'new'>();
  const [secret, setSecret] = useState('');
  const [action, setAction] = useState<{ link: Link; type: 'revoke' | 'rotate' }>();
  const [message, setMessage] = useState('');
  const [error, setError] = useState('');
  return <div className="card">
    <div className="toolbar"><Search placeholder="Search upload links" value={collection.query} onChange={collection.setQuery} />
      <div className="actions"><button onClick={collection.refresh}>Refresh</button><button className="primary" disabled={container.status === 'deleting'} onClick={() => setForm('new')}>＋ Create link</button></div></div>
    <Notice>{message}</Notice><Notice error>{error}</Notice><FetchStatus {...collection} retry={collection.refresh} />
    <p className="small muted">Full URLs are shown only when created or replaced. Received files remain when a link is revoked or replaced.</p>
    {!collection.loading && !collection.error && !collection.data?.items?.length && <Empty>{collection.query ? 'No links match your search.' : 'Create a link for each sender to keep deliveries organized.'}</Empty>}
    {!!collection.data?.items?.length && <div className="table-scroll"><table><thead><tr><th>Sender</th><th>Status</th><th>Expires</th><th>File limit</th><th>Activity</th><th>Actions</th></tr></thead>
      <tbody>{collection.data.items.map((link) => <tr key={link.id}><td><strong>{link.sender_label}</strong><span className="table-subtext mono">{link.id.slice(0, 12)}</span></td>
        <td><Badge status={link.status} /></td><td>{date(link.expires_at)}</td><td>{bytes(link.effective_max_bytes)}<span className="table-subtext">{link.max_file_bytes === null ? 'Inherited' : 'Link override'}</span></td>
        <td>{link.file_count} files<span className="table-subtext">{link.active_uploads} uploading</span></td>
        <td><div className="actions table-actions">
          <button disabled={link.status !== 'active' || container.status === 'deleting'} onClick={() => setForm(link)}>Edit</button>
          <button disabled={container.status === 'deleting' || link.status !== 'active'} onClick={() => setAction({ link, type: 'rotate' })}>Replace</button>
          <button className="danger" disabled={link.status === 'revoked' || container.status === 'deleting'} onClick={() => setAction({ link, type: 'revoke' })}>Revoke</button>
        </div></td></tr>)}</tbody></table></div>}
    {collection.data && <Pagination page={collection.page} total={collection.data.total} limit={LIMIT} onChange={collection.setPage} />}
    {form && <LinkForm initial={form === 'new' ? undefined : form} container={container} settings={settings} onClose={() => setForm(undefined)} onSaved={(result) => {
      collection.refresh();
      if (result.url) setSecret(result.url);
      else if (form === 'new') setError('The link was created but its private URL was missing. Replace the link to obtain a new URL.');
      else setMessage('Link settings saved.');
    }} />}
    {secret && <SecretLink url={secret} onClose={() => { setSecret(''); onChange(); }} />}
    {action && <Confirm title={action.type === 'revoke' ? 'Revoke this link?' : 'Replace this link?'} label={action.type === 'revoke' ? 'Revoke link' : 'Replace link'} onClose={() => setAction(undefined)} action={async () => {
      const result = await api<Link & { url?: string }>(`/api/links/${action.link.id}/${action.type}`, 'POST', {});
      const type = action.type;
      setAction(undefined); collection.refresh();
      if (type === 'rotate') {
        if (!result.url) setError('The link was replaced, but its private URL was missing. Replace it again to recover.');
        else setSecret(result.url);
      } else setMessage('Link revoked. Received files were kept.');
    }}>
      <p><strong>{action.link.sender_label}</strong> · {action.link.active_uploads} active uploads · {action.link.file_count} received files</p>
      <p>{action.type === 'revoke' ? 'The shared URL will stop working immediately.' : 'The old URL and all existing upload sessions will stop working. A new URL will be shown once.'} Unfinished uploads are invalidated. Received files remain.</p>
    </Confirm>}
  </div>;
}

function FilesPanel({ container, onChange }: { container: Container; onChange: () => void }) {
  const collection = useCollection<ReceivedFile>(`/api/containers/${container.id}/files`);
  const [rename, setRename] = useState<ReceivedFile>();
  const [name, setName] = useState('');
  const [deletion, setDeletion] = useState<{ file: ReceivedFile; counts: Container }>();
  const [download, setDownload] = useState<ReceivedFile>();
  const [message, setMessage] = useState('');
  const [error, setError] = useState('');
  async function requestDelete(file: ReceivedFile) {
    setError('');
    try { setDeletion({ file, counts: await api<Container>(`/api/containers/${container.id}`) }); }
    catch (error) { setError(errorMessage(error)); }
  }
  return <div className="card">
    <div className="toolbar"><Search placeholder="Search received files" value={collection.query} onChange={collection.setQuery} /><button onClick={collection.refresh}>Refresh</button></div>
    <Notice>{message}</Notice><Notice error>{error}</Notice><FetchStatus {...collection} retry={collection.refresh} />
    {!collection.loading && !collection.error && !collection.data?.items?.length && <Empty>{collection.query ? 'No files match your search.' : 'No files received yet. Share an upload link with a sender to receive files.'}</Empty>}
    {!!collection.data?.items?.length && <div className="table-scroll"><table><thead><tr><th>File</th><th>Sender</th><th>Size</th><th>Received</th><th>Status</th><th>Actions</th></tr></thead>
      <tbody>{collection.data.items.map((file) => <tr key={file.id}>
        <td className="filename-cell"><strong>{file.name}</strong>{file.original_name !== file.name && <span className="table-subtext">Original: {file.original_name}</span>}
          {file.comment && <details className="file-comment"><summary>Comment</summary><p>{file.comment}</p></details>}</td>
        <td>{file.sender_label}</td><td className="nowrap">{bytes(file.size)}</td><td>{date(file.created_at)}</td><td><Badge status={file.status} /></td>
        <td><div className="actions table-actions">
          {file.status === 'ready' && container.status !== 'deleting' && <button onClick={() => setDownload(file)}>Download</button>}
          <button disabled={file.status !== 'ready' || container.status === 'deleting'} onClick={() => { setRename(file); setName(file.name); }}>Rename</button>
          <button className="danger" disabled={file.status !== 'ready' || container.status === 'deleting'} onClick={() => requestDelete(file)}>Delete</button>
        </div></td></tr>)}</tbody></table></div>}
    {collection.data && <Pagination page={collection.page} total={collection.data.total} limit={LIMIT} onChange={collection.setPage} />}
    {rename && <Modal title="Rename received file" onClose={() => setRename(undefined)}>
      <MutationForm onCancel={() => setRename(undefined)} submit={async () => {
        if (utf8Length(name) > 255) throw new Error('The filename must be no more than 255 UTF-8 bytes.');
        await api(`/api/files/${rename.id}`, 'PUT', { name });
        setRename(undefined); collection.refresh(); setMessage('File renamed. Original filename preserved.');
      }}><label>Display filename<input autoFocus required value={name} onChange={(event) => setName(event.target.value)} /></label>
        <p className="small muted">Original submitted filename: {rename.original_name}. The sender’s comment cannot be edited.</p></MutationForm>
    </Modal>}
    {download && <Modal title="Download this file?" onClose={() => setDownload(undefined)}>
      <p><strong>{download.name}</strong> · {bytes(download.size)}</p>
      <Notice>Received files are untrusted and are not malware-scanned. Only open a file if you trust its source.</Notice>
      <div className="actions form-actions"><button onClick={() => setDownload(undefined)}>Cancel</button>
        <a className="button" href={`/api/files/${download.id}/download`} download onClick={() => {
          setDownload(undefined); setMessage('Download requested. Check your browser’s downloads for progress and any errors.');
        }}>Download file</a></div>
    </Modal>}
    {deletion && <Confirm title="Delete this file?" label="Delete file" onClose={() => setDeletion(undefined)} action={async () => {
      const result = await api<{ status: string }>(`/api/files/${deletion.file.id}`, 'DELETE');
      setDeletion(undefined); collection.refresh();
      setMessage(result.status === 'deleting' ? 'File is deleting. Cleanup is pending; refresh to check its status.' : 'File deleted. Its upload link is unchanged.');
      if (result.status === 'deleted') onChange();
    }}>
      <p>Permanently delete <strong>{deletion.file.name}</strong> ({bytes(deletion.file.size)})? There is no trash.</p>
      <p>This request currently has <strong>{deletion.counts.active_downloads}</strong> active downloads and <strong>{deletion.counts.active_uploads}</strong> active uploads. Counts are request-wide, not specific to this file.</p>
      <p>Already-downloaded bytes cannot be recalled. The sender’s upload link and other received files are unchanged.</p>
    </Confirm>}
  </div>;
}
function AuditPage() {
  const collection = useCollection<Audit>('/api/audit');
  return <>
    <div className="page-heading"><div><p className="eyebrow">Administration</p><h1>Activity</h1><p className="muted">Recent changes and file activity.</p></div></div>
    <section className="card"><div className="toolbar"><Search placeholder="Search activity" value={collection.query} onChange={collection.setQuery} /><button onClick={collection.refresh}>Refresh</button></div>
      <FetchStatus {...collection} retry={collection.refresh} />
      {!collection.loading && !collection.error && !collection.data?.items?.length && <Empty>No matching activity.</Empty>}
      {!!collection.data?.items?.length && <div className="table-scroll"><table><thead><tr><th>Time</th><th>Actor</th><th>Action</th><th>Target</th></tr></thead>
        <tbody>{collection.data.items.map((record) => <tr key={record.id}><td>{date(record.created_at)}</td><td>{record.actor}</td><td>{record.action}</td><td className="mono">{record.target}</td></tr>)}</tbody></table></div>}
      {collection.data && <Pagination page={collection.page} total={collection.data.total} limit={LIMIT} onChange={collection.setPage} />}
    </section>
  </>;
}
