import { Fragment, StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { App } from './App';
import { platform } from './platform';
import './app.css';

const root = document.getElementById('root');
if (root === null) {
  throw new Error('index.html has no #root element');
}

// conchd serves the page on its own origin, so the page's origin is the
// server's. The desktop shell has no server of its own: its Shell asks which
// one first, and the origin is read once that is known.
function Client() {
  return <App origin={platform.services.origin()} />;
}

const Shell = platform.Shell ?? Fragment;

createRoot(root).render(
  <StrictMode>
    <Shell>
      <Client />
    </Shell>
  </StrictMode>,
);
