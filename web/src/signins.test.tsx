import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, onTestFinished, vi } from 'vitest';
import { App } from './App';
import { insecureLine } from './transport';
import { holdShare } from './voice/heldshare';

// "Your sign-ins" in the lobby (issue #496), against a stand-in conchd that
// records each request's method and path.
const origin = 'http://127.0.0.1:8080';
const secret = 'conch_secret_token_value';
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

type Call = { readonly method: string; readonly path: string };
type Handler = (method: string) => Response;

const member = { id: 2, kind: 'human', name: 'ada', role: 'member' };

// Ada's three sign-ins: this browser, one a sign-in link began, and an older
// one from an invite.
const signIns = [
  { id: 9, label: 'browser', created_at: '2026-10-17T09:30:00.000Z', expires_at: '2027-01-15T09:30:00.000Z', renews: true, current: true },
  { id: 8, label: 'sign-in link 3', created_at: '2026-10-17T08:00:00.000Z', expires_at: '2027-01-15T08:00:00.000Z', renews: true },
  { id: 4, label: 'invite 2', created_at: '2026-10-08T12:00:00.000Z' },
];

function serve(routes: Record<string, Handler>) {
  const calls: Call[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const path = typeof input === 'string' ? input : input instanceof URL ? input.pathname : input.url;
      const method = init?.method ?? 'GET';
      calls.push({ method, path });
      if (new Headers(init?.headers).get('Authorization') !== `Bearer ${secret}`) return Promise.resolve(json({}, 401));
      if (path === '/v1/whoami') return Promise.resolve(json(member));
      if (path === '/v1/channels') return Promise.resolve(json({ channels: [] }));
      const handler = routes[path];
      return Promise.resolve(handler === undefined ? json({}, 404) : handler(method));
    }),
  );
  return calls;
}

const listed = (list: unknown[] = signIns) => ({ '/v1/credentials/mine': () => json({ credentials: list }) });

async function signInAndOpen() {
  render(<App origin={origin} />);
  fireEvent.change(screen.getByLabelText('Credential'), { target: { value: secret } });
  fireEvent.click(screen.getByRole('button', { name: 'Use credential' }));
  // Your sign-ins are in Settings (issue #536).
  fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
  const toggle = await screen.findByRole('button', { name: 'Your sign-ins' });
  expect(toggle.getAttribute('aria-expanded')).toBe('false');
  fireEvent.click(toggle);
  expect(toggle.getAttribute('aria-expanded')).toBe('true');
  return screen.getByRole('region', { name: 'Your sign-ins' });
}

// item is the list entry whose label is exactly text.
async function item(region: HTMLElement, text: string): Promise<HTMLElement> {
  const label = await within(region).findByText(text, { selector: 'span' });
  const li = label.closest('li');
  if (li === null) throw new Error(`${text} is not in a list item`);
  return li;
}

const reads = (calls: readonly Call[]) => calls.filter((c) => c.path === '/v1/credentials/mine');
const ends = (calls: readonly Call[]) => calls.filter((c) => c.method === 'DELETE');

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  window.location.hash = '';
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('your sign-ins', () => {
  it('lists every sign-in once opened, marks this browser, and shows a sign-in link that was used', async () => {
    const calls = serve(listed());
    render(<App origin={origin} />);
    fireEvent.change(screen.getByLabelText('Credential'), { target: { value: secret } });
    fireEvent.click(screen.getByRole('button', { name: 'Use credential' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
    await screen.findByRole('button', { name: 'Your sign-ins' });
    // Nothing is read until the person asks.
    expect(reads(calls)).toEqual([]);
    fireEvent.click(screen.getByRole('button', { name: 'Your sign-ins' }));
    const region = screen.getByRole('region', { name: 'Your sign-ins' });

    const list = await within(region).findByRole('list', { name: 'Sign-ins' });
    expect(within(list).getAllByRole('listitem')).toHaveLength(3);
    const here = await item(region, 'browser');
    expect(within(here).getByText('This browser')).toBeDefined();
    const link = await item(region, 'sign-in link 3');
    expect(within(link).queryByText('This browser')).toBeNull();
    expect(within(link).getByText(new Date(signIns[1]?.created_at ?? '').toLocaleString()).getAttribute('datetime')).toBe(signIns[1]?.created_at);
    for (const li of within(list).getAllByRole('listitem')) {
      expect(within(li).getByRole('button', { name: 'End this sign-in' })).toBeDefined();
    }
    expect(reads(calls)).toEqual([{ method: 'GET', path: '/v1/credentials/mine' }]);
  });

  it('ends another sign-in only after a yes, with one DELETE of its id', async () => {
    const calls = serve({ ...listed(), '/v1/credentials/mine/4': () => new Response(null, { status: 204 }) });
    const region = await signInAndOpen();
    const invite = await item(region, 'invite 2');

    fireEvent.click(within(invite).getByRole('button', { name: 'End this sign-in' }));
    const question = within(invite).getByRole('group');
    expect(question.textContent).toContain('End the sign-in “invite 2”?');
    expect(ends(calls)).toEqual([]);
    // Cancel keeps it, and is where focus waits.
    await screen.findByRole('button', { name: 'Cancel' });
    expect(document.activeElement?.textContent).toBe('Cancel');
    fireEvent.click(within(question).getByRole('button', { name: 'Cancel' }));
    expect(within(invite).queryByRole('group')).toBeNull();
    expect(ends(calls)).toEqual([]);

    fireEvent.click(within(invite).getByRole('button', { name: 'End this sign-in' }));
    fireEvent.click(within(invite).getByRole('button', { name: 'Yes, end it' }));
    await within(region).findByText('Ended the sign-in “invite 2”.');
    expect(ends(calls)).toEqual([{ method: 'DELETE', path: '/v1/credentials/mine/4' }]);
    expect(within(region).queryByText('invite 2', { selector: 'span' })).toBeNull();
    expect(within(region).getAllByRole('listitem')).toHaveLength(2);
    // Still signed in here.
    expect(screen.getByRole('navigation', { name: 'Channels' })).toBeDefined();
  });

  it('ends this browser’s sign-in as Sign out does: the same question, then the sign-out', async () => {
    const calls = serve({ ...listed(), '/v1/credentials/current': () => new Response(null, { status: 204 }) });
    const region = await signInAndOpen();
    const here = await item(region, 'browser');
    fireEvent.click(within(here).getByRole('button', { name: 'End this sign-in' }));

    const question = await screen.findByRole('group', { name: /^Sign out of this browser\?/ });
    expect(question.textContent).toContain('you will need a new link from an operator');
    expect(ends(calls)).toEqual([]);
    fireEvent.click(within(question).getByRole('button', { name: 'Yes, sign out' }));
    await screen.findByLabelText('Credential');
    expect(ends(calls)).toEqual([{ method: 'DELETE', path: '/v1/credentials/current' }]);
    expect(window.localStorage.getItem('conch.web.token')).toBeNull();
  });

  it.each([
    ['cannot be reached', (): Response => { throw new TypeError('Failed to fetch'); }, 'Cannot reach the server. The sign-in was not ended; try again.'],
    ['refuses', () => json({ code: 'internal_error', message: 'no' }, 500), 'The server refused. The sign-in was not ended.'],
  ])('keeps the sign-in listed, and says so, when the server %s', async (_case, handler, line) => {
    serve({ ...listed(), '/v1/credentials/mine/8': handler });
    const region = await signInAndOpen();
    const link = await item(region, 'sign-in link 3');
    fireEvent.click(within(link).getByRole('button', { name: 'End this sign-in' }));
    fireEvent.click(within(link).getByRole('button', { name: 'Yes, end it' }));
    await within(region).findByText(line);
    expect(within(region).getByText('sign-in link 3', { selector: 'span' })).toBeDefined();
  });

  it('drops a sign-in the server no longer knows as the caller’s', async () => {
    serve({ ...listed(), '/v1/credentials/mine/8': () => json({ code: 'credential_not_found', message: 'credential not found' }, 404) });
    const region = await signInAndOpen();
    const link = await item(region, 'sign-in link 3');
    fireEvent.click(within(link).getByRole('button', { name: 'End this sign-in' }));
    fireEvent.click(within(link).getByRole('button', { name: 'Yes, end it' }));
    await within(region).findByText('The sign-in “sign-in link 3” had already ended.');
    expect(within(region).queryByText('sign-in link 3', { selector: 'span' })).toBeNull();
  });

  it('says so when the list cannot be read', async () => {
    serve({ '/v1/credentials/mine': () => json({ code: 'internal_error', message: 'no' }, 500) });
    const region = await signInAndOpen();
    expect((await within(region).findByRole('alert')).textContent).toBe('Could not read your sign-ins.');
  });

  it('returns to sign-in when the credential is no longer valid', async () => {
    serve({ '/v1/credentials/mine': () => json({}, 401) });
    await signInAndOpen();
    await screen.findByText('Signed out: the credential is no longer valid');
  });
});

// Add a device (issue #714, design §6.2): a member's own link for another
// device, made after a recent proof of the password, shown only on Reveal
// and never while this tab shares its screen.
describe('add a device', () => {
  const linkCode = `conch_sil_${'D'.repeat(43)}`;
  const accountWho = { ...member, identity_class: 'member', auth: { scope: 'full', password_set: true, setup_complete: true } };
  const link = { id: 21, principal_id: 2, created_by: 2, created_at: '2026-10-23T12:00:00.000Z', expires_at: '2026-10-23T12:10:00.000Z', purpose: 'device', label: 'phone' };

  interface Sent {
    readonly method: string;
    readonly path: string;
    readonly body: unknown;
  }

  // device is ada's server, with a password: the first try to make a link
  // asks for a recent proof (unless proved), and the proof is the one
  // password.
  function device(options: { proved?: boolean; who?: object } = {}) {
    const sent: Sent[] = [];
    let proved = options.proved ?? false;
    let open: unknown[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
        const path = typeof input === 'string' ? input : input instanceof URL ? input.pathname : input.url;
        const method = init?.method ?? 'GET';
        const body: unknown = typeof init?.body === 'string' ? JSON.parse(init.body) : undefined;
        sent.push({ method, path, body });
        if (new Headers(init?.headers).get('Authorization') !== `Bearer ${secret}`) return Promise.resolve(json({}, 401));
        if (path === '/v1/whoami') return Promise.resolve(json(options.who ?? accountWho));
        if (path === '/v1/channels') return Promise.resolve(json({ channels: [] }));
        if (path === '/v1/credentials/mine') return Promise.resolve(json({ credentials: signIns }));
        if (path === '/v1/sign-in-links/mine' && method === 'GET') return Promise.resolve(json({ sign_in_links: open }));
        if (path === '/v1/sign-in-links/mine' && method === 'POST') {
          if (!proved) return Promise.resolve(json({ code: 'reauth_required', message: 'needs a recent proof' }, 403));
          open = [link];
          return Promise.resolve(json({ sign_in_link: link, code: linkCode }, 201));
        }
        if (path === '/v1/sign-in-links/mine/21' && method === 'DELETE') {
          open = [];
          return Promise.resolve(new Response(null, { status: 204 }));
        }
        if (path === '/v1/reauth') {
          if ((body as { password?: string }).password !== 'ada’s own long passphrase') return Promise.resolve(json({ code: 'auth_failed', message: 'no' }, 401));
          proved = true;
          return Promise.resolve(json({ until: '2026-10-23T12:10:00.000Z' }));
        }
        return Promise.resolve(json({}, 404));
      }),
    );
    return sent;
  }

  async function openAddDevice() {
    const region = await signInAndOpen();
    return within(region).findByRole('region', { name: 'Add a device' });
  }

  it('asks for the password first, then makes the link, hidden until Reveal, and forgets it on Done', async () => {
    const sent = device();
    const writes: string[] = [];
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: {
        writeText: (text: string) => {
          writes.push(text);
          return Promise.resolve();
        },
      },
    });
    onTestFinished(() => {
      Reflect.deleteProperty(navigator, 'clipboard');
    });
    const container = document.body;
    const section = await openAddDevice();
    fireEvent.change(within(section).getByLabelText('Name for the device (optional)'), { target: { value: 'phone' } });
    fireEvent.click(within(section).getByRole('button', { name: 'Make a device link' }));

    const dialog = await screen.findByRole('dialog', { name: 'Confirm it is you' });
    expect(within(dialog).queryByLabelText('Code from your authenticator app')).toBeNull();
    // What was typed in the form is kept underneath.
    expect(within(section).getByLabelText<HTMLInputElement>('Name for the device (optional)').value).toBe('phone');
    fireEvent.change(within(dialog).getByLabelText('Your password'), { target: { value: 'ada’s own long passphrase' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Confirm' }));

    const made = await within(section).findByRole('group', { name: 'New device link' });
    expect(made.textContent).toContain('It is shown only here, and only now.');
    expect(within(made).getByText(/it cannot know about another tab, another app/)).toBeDefined();
    expect(container.innerHTML).not.toContain(linkCode);
    expect(sent.filter((c) => c.path === '/v1/sign-in-links/mine' && c.method === 'POST').map((c) => c.body)).toEqual([{ label: 'phone' }, { label: 'phone' }]);

    fireEvent.click(within(made).getByRole('button', { name: 'Copy link' }));
    await within(made).findByText('Copied.');
    expect(writes).toEqual([`${origin}/app/#/signin/${linkCode}`]);
    expect(container.innerHTML).not.toContain(linkCode);

    fireEvent.click(within(made).getByRole('button', { name: 'Reveal link' }));
    expect(within(made).getByLabelText<HTMLInputElement>('Device link').value).toBe(`${origin}/app/#/signin/${linkCode}`);
    fireEvent.click(within(made).getByRole('button', { name: 'Hide link' }));
    expect(within(made).queryByLabelText('Device link')).toBeNull();

    fireEvent.click(within(made).getByRole('button', { name: 'Done' }));
    expect(within(section).queryByRole('group', { name: 'New device link' })).toBeNull();
    expect(container.innerHTML).not.toContain(linkCode);
    // It is listed as open, without its code, and Close ends it.
    const list = await within(section).findByRole('list', { name: 'Open device links' });
    fireEvent.click(within(list).getByRole('button', { name: 'Close link' }));
    await within(section).findByText('None open.');
    expect(sent.some((c) => c.method === 'DELETE' && c.path === '/v1/sign-in-links/mine/21')).toBe(true);
  });

  it('refuses to reveal the link while this tab shares its screen, and hides it when a share starts', async () => {
    device({ proved: true });
    const section = await openAddDevice();
    fireEvent.click(within(section).getByRole('button', { name: 'Make a device link' }));
    const made = await within(section).findByRole('group', { name: 'New device link' });
    fireEvent.click(within(made).getByRole('button', { name: 'Reveal link' }));
    expect(within(made).getByLabelText('Device link')).toBeDefined();

    const share = await holdShare();
    await act(async () => {});
    expect(within(made).queryByLabelText('Device link')).toBeNull();
    expect(within(made).getByText(/This tab is sharing its screen/)).toBeDefined();
    expect(within(made).getByRole<HTMLButtonElement>('button', { name: 'Reveal link' }).disabled).toBe(true);
    expect(document.body.innerHTML).not.toContain(linkCode);

    await act(async () => {
      await share.stop();
    });
    fireEvent.click(within(made).getByRole('button', { name: 'Reveal link' }));
    expect(within(made).getByLabelText('Device link')).toBeDefined();
  });

  it('makes no link on a page served over plain HTTP beyond this machine, since it asks for the password', async () => {
    device();
    render(<App origin="http://192.168.1.20:8080" />);
    fireEvent.change(screen.getByLabelText('Credential'), { target: { value: secret } });
    fireEvent.click(screen.getByRole('button', { name: 'Use credential' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Your sign-ins' }));
    const section = await screen.findByRole('region', { name: 'Add a device' });
    expect(within(section).getByText(insecureLine)).toBeDefined();
    expect(within(section).queryByRole('button', { name: 'Make a device link' })).toBeNull();
  });

  // Security review N5: only the routes that take a password or a code are
  // believed when they answer 401 auth_failed; on any other, it is the
  // credential refused, and the page signs out.
  it('signs out on a 401 auth_failed from a route that takes no password', async () => {
    device();
    const real = globalThis.fetch;
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
        const path = typeof input === 'string' ? input : input instanceof URL ? input.pathname : input.url;
        if (path === '/v1/sign-in-links/mine' && init?.method === 'POST') return Promise.resolve(json({ code: 'auth_failed', message: 'no' }, 401));
        return real(input, init);
      }),
    );
    const section = await openAddDevice();
    fireEvent.click(within(section).getByRole('button', { name: 'Make a device link' }));
    await screen.findByText('Signed out: the credential is no longer valid');
    expect(window.localStorage.getItem('conch.web.token')).toBeNull();
    expect(screen.queryByRole('dialog', { name: 'Confirm it is you' })).toBeNull();
  });

  it('is not offered to an operator', async () => {
    device({ who: { ...accountWho, role: 'operator' } });
    window.sessionStorage.setItem('conch.web.token', secret);
    render(<App origin={origin} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Your sign-ins' }));
    await within(screen.getByRole('region', { name: 'Your sign-ins' })).findByRole('list', { name: 'Sign-ins' });
    expect(screen.queryByRole('heading', { name: 'Add a device' })).toBeNull();
  });
});
