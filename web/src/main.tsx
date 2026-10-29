import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { App } from './App';
import { platform } from './platform';
import './app.css';

const root = document.getElementById('root');
if (root === null) {
  throw new Error('index.html has no #root element');
}

// conchd serves the page on its own origin, so the page's origin is the
// server's.
createRoot(root).render(
  <StrictMode>
    <App origin={platform.services.origin()} />
  </StrictMode>,
);
