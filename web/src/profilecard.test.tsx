import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { ProfileCard } from './ProfileCard';
import type { Entry } from './agents/join';
import type { Person } from './useLiveChannel';

const ada: Person = { name: 'ada', kind: 'human', role: 'member', manages: true };
const online: Entry = { id: 1, name: 'ada', kind: 'human', sessions: [], live: 2, state: 'stale', status: 'idle' };

function open(name = 'ada'): HTMLElement {
  const opener = screen.getByRole('button', { name });
  fireEvent.click(opener);
  return screen.getByRole('dialog', { name });
}

afterEach(cleanup);

describe('the read-only profile card', () => {
  it('shows the held identity and Entry presence, with Close as its only control', () => {
    render(<ProfileCard id={1} person={{ ...online, ...ada }} viewer={2} />);
    const card = open();
    expect(card.getAttribute('aria-modal')).toBe('false');
    expect(card.querySelector('.avatar')).not.toBeNull();
    expect(within(card).getByText('Person')).toBeDefined();
    expect(within(card).getByText('Manager')).toBeDefined();
    expect(within(card).getByText('stale (2 sessions)')).toBeDefined();
    expect(within(card).getAllByRole('button').map((button) => button.textContent)).toEqual(['Close']);
    expect(document.activeElement).toBe(within(card).getByRole('button', { name: 'Close' }));
  });

  it('shows no Owner mark when the opening surface was not sent owner', () => {
    const ownerWithoutOwnerField: Person = { name: 'owner', kind: 'human', role: 'member', manages: false };
    render(<ProfileCard id={1} person={ownerWithoutOwnerField} viewer={2} />);
    const card = open('owner');
    expect(within(card).queryByText('Owner')).toBeNull();
    expect(card.querySelector('[data-identity="owner"]')).toBeNull();
    expect(card.querySelector('.profile-card-presence')).toBeNull();
  });

  it.each([
    ['Escape', (card: HTMLElement) => { fireEvent.keyDown(card, { key: 'Escape' }); }],
    ['Close', (card: HTMLElement) => { fireEvent.click(within(card).getByRole('button', { name: 'Close' })); }],
    ['Tab past its end', (card: HTMLElement) => { fireEvent.keyDown(within(card).getByRole('button', { name: 'Close' }), { key: 'Tab' }); }],
    ['an outside press', () => { fireEvent.pointerDown(document.body); }],
  ])('closes on %s and returns focus to the name', (_how, leave) => {
    render(<ProfileCard id={1} person={ada} viewer={2} />);
    const opener = screen.getByRole('button', { name: 'ada' });
    const card = open();
    leave(card);
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(document.activeElement).toBe(opener);
  });

  it('keeps only one card open', () => {
    render(<><ProfileCard id={1} person={ada} viewer={2} /><ProfileCard id={2} person={{ name: 'bot', kind: 'agent' }} viewer={2} /></>);
    open();
    open('bot');
    expect(screen.getAllByRole('dialog')).toHaveLength(1);
    expect(screen.getByRole('dialog', { name: 'bot' })).toBeDefined();
  });

  it('lets one outside tap close the card and activate another name', () => {
    render(<><ProfileCard id={1} person={ada} viewer={2} /><ProfileCard id={2} person={{ name: 'bot', kind: 'agent' }} viewer={2} /></>);
    open();
    const bot = screen.getByRole('button', { name: 'bot' });
    expect(fireEvent.touchStart(bot, { touches: [{ clientX: 10, clientY: 10 }] })).toBe(true);
    fireEvent.touchEnd(bot);
    fireEvent.click(bot);
    expect(screen.queryByRole('dialog', { name: 'ada' })).toBeNull();
    expect(screen.getByRole('dialog', { name: 'bot' })).toBeDefined();
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Close' }));
  });

  it('closes when the opening surface loses the person, and returns focus to the fallback name', () => {
    const view = render(<ProfileCard id={1} person={ada} viewer={2} />);
    const opener = screen.getByRole('button', { name: 'ada' });
    open();
    view.rerender(<ProfileCard id={1} person={undefined} viewer={2} />);
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(document.activeElement).toBe(opener);
    expect(opener.textContent).toBe('#1');
  });

  it('opens an unnamed person as #<id> without inventing a kind, marks or presence', () => {
    render(<ProfileCard id={7} person={undefined} viewer={2} />);
    const card = open('#7');
    expect(card.querySelector('.profile-card-kind')).toBeNull();
    expect(within(card).queryByText('Person')).toBeNull();
    expect(within(card).queryByText('Agent')).toBeNull();
    expect(card.querySelectorAll('.identity-mark')).toHaveLength(0);
    expect(card.querySelector('.profile-card-presence')).toBeNull();
  });
});
