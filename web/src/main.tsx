import { Component, StrictMode, useEffect, useState, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';
import { api, errorMessage } from './api';
import Admin from './Admin';
import Public from './Public';
import { Brand, Empty, Notice } from './ui';
import './styles.css';

class ErrorBoundary extends Component<{ children: ReactNode }, { failed: boolean }> {
  state = { failed: false };
  static getDerivedStateFromError() { return { failed: true }; }
  render() {
    if (this.state.failed) return <><Brand /><main className="public-main"><Notice error>The page encountered an unexpected error. Reload to recover. Any selected local files will need to be selected again.</Notice><button onClick={() => window.location.reload()}>Reload</button></main></>;
    return this.props.children;
  }
}
function App() {
  const [surface, setSurface] = useState<'admin' | 'public'>();
  const [error, setError] = useState('');
  const [version, setVersion] = useState(0);
  useEffect(() => {
    let current = true;
    setError('');
    api<{ surface: 'admin' | 'public' }>('/api/surface').then((result) => {
      if (result.surface !== 'admin' && result.surface !== 'public') throw new Error('The server returned an unknown application surface.');
      if (current) setSurface(result.surface);
    }).catch((error) => { if (current) setError(errorMessage(error)); });
    return () => { current = false; };
  }, [version]);
  const id = /^\/u\/([a-f0-9]+)\/?$/.exec(window.location.pathname)?.[1];
  if (surface === 'public' && id) return <Public id={id} />;
  if (surface === 'admin' && !window.location.pathname.startsWith('/u/')) return <Admin />;
  if (surface) return <><Brand href={surface === 'admin' ? '/' : undefined} /><main className="public-main"><Empty><h1>Page not found</h1><p>{surface === 'public' ? 'Open the complete upload link shared with you.' : 'Upload links must be opened on the public upload address.'}</p></Empty></main></>;
  return <><Brand /><main className="public-main"><Notice error>{error}</Notice>{error ? <button onClick={() => setVersion((value) => value + 1)}>Retry</button> : <Empty>Opening upfile…</Empty>}</main></>;
}

createRoot(document.getElementById('root')!).render(<StrictMode><ErrorBoundary><App /></ErrorBoundary></StrictMode>);
