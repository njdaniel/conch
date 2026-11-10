// "Open in the desktop app" on the invite landing page (issue #1592, D5-06),
// and the conch:// link it makes. conchd is a stand-in fetch.
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';
import { platform } from './platform';
import { desktopInviteLink, inviteLink } from './route';

const origin = 'https://chat.example.org';
const inviteCode = `conch_inv_${'Q'.repeat(40)}x-_`;
const expires_at = '2026-10-23T12:00:00Z';
const preview = { kind: 'member', server_name: 'Conch', inviter: 'ada', channels: ['red'], expires_at, join: { guest: false, account: true } };

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

function serve(previewReply: Response): void {
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const path = typeof input === 'string' ? input : input instanceof URL ? input.pathname : input.url;
      const method = init?.method ?? 'GET';
      return Promise.resolve(`${method} ${path}` === 'POST /v2/invites/preview' ? previewReply.clone() : json({}, 404));
    }),
  );
}

function opened() {
  window.location.hash = `#/invite/${inviteCode}`;
  return render(<App origin={origin} />);
}

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  window.location.hash = '';
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe('desktopInviteLink', () => {
  it('is conch://invite?server=<percent-encoded origin>&code=<code>', () => {
    expect(desktopInviteLink('https://chat.example.org', 'abc-DEF_1.2~3')).toBe('conch://invite?server=https%3A%2F%2Fchat.example.org&code=abc-DEF_1.2~3');
    expect(desktopInviteLink('http://127.0.0.1:8080', 'abc')).toBe('conch://invite?server=http%3A%2F%2F127.0.0.1%3A8080&code=abc');
  });

  it('percent-encodes every character that could end a value or start another parameter', () => {
    const link = desktopInviteLink('https://chat.example.org', 'a&server=evil#x y/z?+=%');
    expect(link).toBe('conch://invite?server=https%3A%2F%2Fchat.example.org&code=a%26server%3Devil%23x%20y%2Fz%3F%2B%3D%25');
    // Exactly two parameters, and nothing after a raw "#".
    expect(link.split('?')[1]?.split('&')).toHaveLength(2);
    expect(link).not.toContain('#');
  });

  it('names the same server and code as the https link', () => {
    const https = new URL(inviteLink(origin, inviteCode).replace('#', '?x#'));
    const scheme = new URLSearchParams(desktopInviteLink(origin, inviteCode).split('?')[1]);
    expect(scheme.get('server')).toBe(https.origin);
    expect(scheme.get('code')).toBe(decodeURIComponent(https.hash.slice('#/invite/'.length)));
  });
});

describe('the invite page in a browser', () => {
  it('offers "Open in the desktop app" as a secondary action, never by itself, with the link in neither the page nor the address', async () => {
    serve(json(preview));
    const open = vi.spyOn(window, 'open').mockReturnValue(null);
    const view = opened();
    const button = await screen.findByRole('button', { name: 'Open in the desktop app' });
    // The join is still there, first; this is beside it and not a link.
    expect(screen.getByLabelText('Your name')).toBeDefined();
    expect(button.tagName).toBe('BUTTON');
    expect(button.closest('a')).toBeNull();
    expect(button.getAttribute('class')).toBe('link');
    // Nothing opened or navigated by itself, and the code is nowhere in the page.
    expect(open).not.toHaveBeenCalled();
    expect(view.container.innerHTML).not.toContain(inviteCode);
    expect(view.container.innerHTML).not.toContain('conch://');
    expect(window.location.href).not.toContain(inviteCode);

    fireEvent.click(button);
    expect(open).toHaveBeenCalledExactlyOnceWith(desktopInviteLink(origin, inviteCode), '_self');
    // The page is where it was: still the invite, still joinable.
    expect(screen.getByLabelText('Your name')).toBeDefined();
    expect(view.container.innerHTML).not.toContain(inviteCode);
  });

  it('is not shown for an invite that cannot be used, or while the preview is loading', async () => {
    serve(json({ code: 'invite_invalid', message: 'gone' }, 404));
    opened();
    await screen.findByRole('heading', { name: "This link can't be used" });
    expect(screen.queryByRole('button', { name: 'Open in the desktop app' })).toBeNull();
  });

  it('is not shown where the platform cannot hand a link to an installed app (the desktop app itself)', async () => {
    serve(json(preview));
    const saved = Object.getOwnPropertyDescriptor(platform.services, 'openDesktopApp');
    delete platform.services.openDesktopApp;
    try {
      opened();
      await screen.findByLabelText('Your name');
      expect(screen.queryByRole('button', { name: 'Open in the desktop app' })).toBeNull();
    } finally {
      if (saved !== undefined) Object.defineProperty(platform.services, 'openDesktopApp', saved);
    }
  });
});
