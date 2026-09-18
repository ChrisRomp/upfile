import { useEffect, useRef, useState, type FormEvent, type ReactNode } from 'react';
import { errorMessage } from './api';
import { BRAND } from './format';

export function Brand({ children, href }: { children?: ReactNode; href?: string }) {
  const name = <>{BRAND}<span aria-hidden="true">↥</span></>;
  return <header className="site-header">{href ? <a className="brand" href={href}>{name}</a> : <span className="brand">{name}</span>}{children}</header>;
}
export function Notice({ children, error = false }: { children?: ReactNode; error?: boolean }) {
  return children ? <div className={`notice ${error ? 'error' : ''}`} role={error ? 'alert' : 'status'}>{children}</div> : null;
}
export function Empty({ children }: { children: ReactNode }) {
  return <div className="empty">{children}</div>;
}
export function Badge({ status }: { status: string }) {
  return <span className={`badge ${status === 'active' || status === 'ready' || status === 'completed' ? 'good' : ''}`}>{status}</span>;
}
export function Pagination({ page, total, limit, onChange }: {
  page: number; total: number; limit: number; onChange: (page: number) => void;
}) {
  return <nav className="pagination" aria-label="Pagination">
    <span>{total.toLocaleString()} total · Page {page} of {Math.max(1, Math.ceil(total / limit))}</span>
    <div className="actions">
      <button disabled={page <= 1} onClick={() => onChange(page - 1)}>Previous</button>
      <button disabled={page * limit >= total} onClick={() => onChange(page + 1)}>Next</button>
    </div>
  </nav>;
}
export function Search({ placeholder, value, onChange }: {
  placeholder: string; value: string; onChange: (value: string) => void;
}) {
  return <label className="search"><span className="sr-only">{placeholder}</span>
    <input type="search" placeholder={placeholder} value={value} onChange={(event) => onChange(event.target.value)} />
  </label>;
}
export function Modal({ title, children, onClose }: {
  title: string; children: ReactNode; onClose: () => void;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const previous = document.activeElement as HTMLElement | null;
    ref.current?.showModal();
    return () => { ref.current?.close(); previous?.focus(); };
  }, []);
  return <dialog ref={ref} onCancel={(event) => { event.preventDefault(); onClose(); }} aria-labelledby="dialog-title">
    <div className="dialog-heading"><h2 id="dialog-title">{title}</h2><button type="button" aria-label="Close dialog" onClick={onClose}>×</button></div>
    {children}
  </dialog>;
}
export function MutationForm({ submit, label = 'Save changes', children, onCancel }: {
  submit: () => Promise<void>; label?: string; children: ReactNode; onCancel?: () => void;
}) {
  const [pending, setPending] = useState(false);
  const [error, setError] = useState('');
  async function handleSubmit(event: FormEvent) {
    event.preventDefault();
    if (pending) return;
    setPending(true); setError('');
    try { await submit(); } catch (error) { setError(errorMessage(error)); }
    finally { setPending(false); }
  }
  return <form onSubmit={handleSubmit} className="form-stack">
    <fieldset disabled={pending}>{children}</fieldset>
    <Notice error>{error}</Notice>
    <div className="actions form-actions">
      {onCancel && <button type="button" disabled={pending} onClick={onCancel}>Cancel</button>}
      <button type="submit" className="primary" disabled={pending}>{pending ? 'Working…' : label}</button>
    </div>
  </form>;
}
export function Confirm({ title, children, requiredName, action, onClose, label = 'Confirm' }: {
  title: string; children: ReactNode; requiredName?: string;
  action: () => Promise<void>; onClose: () => void; label?: string;
}) {
  const [name, setName] = useState('');
  return <Modal title={title} onClose={onClose}>
    <MutationForm onCancel={onClose} label={label} submit={async () => {
      if (requiredName && name !== requiredName) throw new Error('The name must match exactly.');
      await action();
    }}>
      {children}
      {requiredName && <label>Type <strong>{requiredName}</strong> to confirm<input value={name} onChange={(event) => setName(event.target.value)} autoComplete="off" required /></label>}
    </MutationForm>
  </Modal>;
}
