// The opt-in tray (issue #1589): the state mapping as a table, and the
// controller against a fake Tray, Menu, bus and voice service.
import { existsSync } from 'node:fs';
import { join } from 'node:path';

import { describe, expect, it } from 'vitest';

import type { NativeVoiceHelper, NativeVoiceView, VoiceCommandResult } from '../../../web/src/platform/desktop/bridge.ts';
import type { AppSettings } from '../../main/settings.ts';
import { iconFile, trayModel, TrayController, type TrayDeps, type TrayIcon, type TrayLike, type TrayMenuItem } from '../../main/tray.ts';

const helper = (over: Partial<NativeVoiceHelper> = {}): NativeVoiceHelper => ({
  version: 1,
  phase: 'connected',
  capture: 'ready',
  transmitting: false,
  muted: false,
  deafened: false,
  blocked: [],
  ptt: 'ready',
  ...over,
}) as unknown as NativeVoiceHelper;

const IDLE: NativeVoiceView = { status: 'idle', generation: 0, channel: null, retryable: false };
const running = (over: Partial<NativeVoiceHelper> = {}, channel: string | null = 'general'): NativeVoiceView => ({ status: 'running', generation: 1, channel, retryable: false, helper: helper(over) });

describe('trayModel', () => {
  const cases: readonly [string, Parameters<typeof trayModel>[0], Partial<ReturnType<typeof trayModel>>][] = [
    ['not in voice', { profile: 'Home', voice: IDLE, sharing: false, pending: false }, { icon: 'idle', header: 'Home', voiceLine: 'Not in voice', stateLine: null, mute: { label: 'Mute', enabled: false }, leave: { label: 'Leave voice', enabled: false } }],
    ['no profile', { profile: null, voice: IDLE, sharing: false, pending: false }, { header: 'Conch' }],
    ['in voice, quiet', { profile: 'Home', voice: running(), sharing: false, pending: false }, { icon: 'voice', voiceLine: 'In voice: #general', stateLine: null, mute: { label: 'Mute', enabled: true }, deafen: { label: 'Deafen', enabled: true }, leave: { label: 'Leave voice', enabled: true } }],
    ['transmitting only when the helper says so', { profile: 'Home', voice: running({ transmitting: true }), sharing: false, pending: false }, { icon: 'transmitting', stateLine: 'Transmitting' }],
    ['muted', { profile: 'Home', voice: running({ muted: true }), sharing: false, pending: false }, { icon: 'voice', stateLine: 'Muted', mute: { label: 'Unmute', enabled: true } }],
    ['deafened', { profile: 'Home', voice: running({ muted: true, deafened: true }), sharing: false, pending: false }, { stateLine: 'Deafened', deafen: { label: 'Undeafen', enabled: true } }],
    ['sharing wins', { profile: 'Home', voice: running({ transmitting: true }), sharing: true, pending: false }, { icon: 'sharing', stateLine: 'Sharing your screen' }],
    ['connecting is not in voice', { profile: 'Home', voice: running({ phase: 'connecting' }), sharing: false, pending: false }, { icon: 'idle', voiceLine: 'Not in voice' }],
    ['a failed helper is not in voice', { profile: 'Home', voice: { status: 'failed', generation: 1, channel: 'general', problem: 'exited', retryable: true }, sharing: false, pending: false }, { icon: 'idle', voiceLine: 'Not in voice', stateLine: null }],
    ['a pending command disables the actions and changes no label', { profile: 'Home', voice: running({ muted: true }), sharing: false, pending: true }, { mute: { label: 'Unmute', enabled: false }, deafen: { label: 'Deafen', enabled: false }, leave: { label: 'Leave voice', enabled: false }, stateLine: 'Muted' }],
  ];
  it.each(cases)('%s', (_name, input, want) => {
    expect(trayModel(input)).toMatchObject(want);
  });
});

describe('the icons', () => {
  it('has a 22 px and a @2x file for each state, and they are packaged', () => {
    const dir = join(import.meta.dirname, '../../build/icons/tray');
    for (const state of ['idle', 'voice', 'transmitting', 'sharing'] as const satisfies readonly TrayIcon[]) {
      const file = iconFile(state);
      expect(existsSync(join(dir, file))).toBe(true);
      expect(existsSync(join(dir, file.replace('.png', '@2x.png')))).toBe(true);
    }
  });
});

// ------------------------------------------------------------ the controller

interface Rig {
  deps: TrayDeps;
  trays: FakeTray[];
  settings: AppSettings;
  state: { bus: boolean; confirmed: boolean; hidden: boolean; view: NativeVoiceView; sharing: boolean; profile: string | null };
  shown: number;
  quit: number;
  revoked: number;
  logs: string[];
  commands: string[];
  next: VoiceCommandResult;
  listeners: ((v: NativeVoiceView) => void)[];
  tick: () => void;
  stopped: number;
}

class FakeTray implements TrayLike {
  image = '';
  menu: readonly TrayMenuItem[] = [];
  destroyed = false;
  click: (() => void) | null = null;
  setImage(path: string): void {
    this.image = path;
  }
  setToolTip(): void {}
  setContextMenu(menu: unknown): void {
    this.menu = menu as readonly TrayMenuItem[];
  }
  on(_event: 'click', listener: () => void): void {
    this.click = listener;
  }
  destroy(): void {
    this.destroyed = true;
  }
}

function rig(closeToTray = true): Rig {
  const r: Rig = {
    trays: [],
    settings: { closeToTray, launchAtLogin: false },
    state: { bus: true, confirmed: true, hidden: false, view: IDLE, sharing: false, profile: 'Home' },
    shown: 0,
    quit: 0,
    revoked: 0,
    logs: [],
    commands: [],
    next: { ok: true },
    listeners: [],
    tick: () => undefined,
    stopped: 0,
    deps: undefined as unknown as TrayDeps,
  };
  r.deps = {
    createTray: () => {
      const t = new FakeTray();
      r.trays.push(t);
      return t;
    },
    buildMenu: (items) => items,
    iconDir: '/icons',
    voice: {
      state: () => r.state.view,
      subscribe: (l) => {
        r.listeners.push(l);
        return () => {
          r.listeners = r.listeners.filter((x) => x !== l);
        };
      },
      setMute: (m) => {
        r.commands.push(`mute ${String(m)}`);
        return Promise.resolve(r.next);
      },
      setDeafen: (d) => {
        r.commands.push(`deafen ${String(d)}`);
        return Promise.resolve(r.next);
      },
      leave: () => {
        r.commands.push('leave');
        return Promise.resolve(IDLE);
      },
    },
    sharing: () => r.state.sharing,
    profile: () => r.state.profile,
    show: () => {
      r.shown++;
      r.state.hidden = false;
    },
    hidden: () => r.state.hidden,
    quit: () => {
      r.quit++;
    },
    detect: () => Promise.resolve(r.state.bus),
    revoke: () => {
      r.revoked++;
      r.state.confirmed = false;
    },
    settings: () => r.settings,
    confirmed: () => r.state.confirmed,
    every: (fn) => {
      r.tick = fn;
      return () => {
        r.stopped++;
      };
    },
    log: (l) => r.logs.push(l),
  };
  return r;
}

const emit = (r: Rig): void => {
  for (const l of r.listeners) l(r.state.view);
};
const labels = (t: FakeTray): string[] => t.menu.map((i) => i.label ?? '---');
const item = (t: FakeTray, label: string): TrayMenuItem => {
  const found = t.menu.find((i) => i.label === label);
  if (found === undefined) throw new Error(`no menu item ${label}`);
  return found;
};

describe('the tray exists only while closeToTray is on', () => {
  it('makes no tray with it off', async () => {
    const r = rig(false);
    const c = new TrayController(r.deps);
    await c.sync();
    expect(r.trays).toEqual([]);
    expect(c.working()).toBe(false);
    expect(c.exists).toBe(false);
  });

  it('creates one when it goes on, and destroys it and stops the timer when it goes off', async () => {
    const r = rig(false);
    const c = new TrayController(r.deps);
    r.settings = { ...r.settings, closeToTray: true };
    await c.sync();
    expect(r.trays).toHaveLength(1);
    expect(c.working()).toBe(true);
    r.settings = { ...r.settings, closeToTray: false };
    await c.sync();
    expect(r.trays[0]?.destroyed).toBe(true);
    expect(r.stopped).toBe(1);
    expect(c.working()).toBe(false);
    expect(r.listeners).toEqual([]);
  });

  it('is not working until confirmed, or while the host is not on the bus', async () => {
    const r = rig();
    r.state.confirmed = false;
    const c = new TrayController(r.deps);
    await c.sync();
    expect(c.working()).toBe(false);
    r.state.confirmed = true;
    expect(c.working()).toBe(true);
    r.state.bus = false;
    r.state.confirmed = true;
    await c.check();
    expect(c.working()).toBe(false);
  });
});

describe('the menu', () => {
  it('reads Not in voice with the profile, and offers Show, the voice actions disabled, and Quit', async () => {
    const r = rig();
    await new TrayController(r.deps).sync();
    const t = r.trays[0] as FakeTray;
    expect(labels(t)).toEqual(['Home', 'Not in voice', '---', 'Show Conch', 'Mute', 'Deafen', 'Leave voice', '---', 'Quit']);
    expect(item(t, 'Mute').enabled).toBe(false);
    expect(item(t, 'Leave voice').enabled).toBe(false);
    expect(t.image).toBe('/icons/idle.png');
  });

  it('follows the helper confirmed state, and nothing else', async () => {
    const r = rig();
    await new TrayController(r.deps).sync();
    const t = r.trays[0] as FakeTray;
    r.state.view = running({ transmitting: true });
    emit(r);
    expect(labels(t).slice(0, 3)).toEqual(['Home', 'In voice: #general', 'Transmitting']);
    expect(t.image).toBe('/icons/transmitting.png');
    r.state.sharing = true;
    emit(r);
    expect(labels(t)[2]).toBe('Sharing your screen');
    expect(t.image).toBe('/icons/sharing.png');
  });

  it('shows the profile in force at each rebuild, and nothing of the old one once media ends', async () => {
    const r = rig();
    const c = new TrayController(r.deps);
    await c.sync();
    const t = r.trays[0] as FakeTray;
    r.state.view = running();
    emit(r);
    expect(labels(t)[0]).toBe('Home');
    c.blank();
    expect(labels(t).slice(0, 2)).toEqual(['Conch', 'Not in voice']);
    r.state.profile = 'Work';
    r.state.view = IDLE;
    c.refresh();
    expect(labels(t)[0]).toBe('Work');
  });

  it('Show and a click on the icon show the window', async () => {
    const r = rig();
    await new TrayController(r.deps).sync();
    const t = r.trays[0] as FakeTray;
    item(t, 'Show Conch').click?.();
    t.click?.();
    expect(r.shown).toBe(2);
  });

  it('Quit asks the app to quit', async () => {
    const r = rig();
    await new TrayController(r.deps).sync();
    item(r.trays[0] as FakeTray, 'Quit').click?.();
    expect(r.quit).toBe(1);
  });
});

describe('menu commands', () => {
  it('call VoiceService, and the label changes only on the helper next state', async () => {
    const r = rig();
    await new TrayController(r.deps).sync();
    const t = r.trays[0] as FakeTray;
    r.state.view = running();
    emit(r);
    item(t, 'Mute').click?.();
    await Promise.resolve();
    await Promise.resolve();
    expect(r.commands).toEqual(['mute true']);
    // The command was acknowledged, but the helper has not reported: still Mute.
    expect(labels(t)).toContain('Mute');
    expect(labels(t)).not.toContain('Unmute');
    r.state.view = running({ muted: true });
    emit(r);
    expect(labels(t)).toContain('Unmute');
    item(t, 'Unmute').click?.();
    await Promise.resolve();
    expect(r.commands).toEqual(['mute true', 'mute false']);
  });

  it('disable the actions while one waits, with no "Muting..." label', async () => {
    const r = rig();
    await new TrayController(r.deps).sync();
    const t = r.trays[0] as FakeTray;
    r.state.view = running();
    emit(r);
    let release: (v: VoiceCommandResult) => void = () => undefined;
    (r.deps.voice as { setMute: (m: boolean) => Promise<VoiceCommandResult> }).setMute = () =>
      new Promise((resolve) => {
        release = resolve;
      });
    item(t, 'Mute').click?.();
    expect(item(t, 'Mute').enabled).toBe(false);
    expect(item(t, 'Deafen').enabled).toBe(false);
    expect(labels(t).join('|')).not.toMatch(/ing/u);
    release({ ok: true });
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(item(t, 'Mute').enabled).toBe(true);
  });

  it('a failed command leaves the label and logs the reason alone', async () => {
    const r = rig();
    await new TrayController(r.deps).sync();
    const t = r.trays[0] as FakeTray;
    r.state.view = running({ muted: true });
    emit(r);
    r.next = { ok: false, error: 'lost' };
    item(t, 'Unmute').click?.();
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(labels(t)).toContain('Unmute');
    expect(r.logs).toEqual(['tray: mute failed (lost)']);
    expect(r.logs.join()).not.toContain('Home');
    expect(r.logs.join()).not.toContain('general');
  });

  it('Deafen and Leave call VoiceService', async () => {
    const r = rig();
    await new TrayController(r.deps).sync();
    const t = r.trays[0] as FakeTray;
    r.state.view = running();
    emit(r);
    item(t, 'Deafen').click?.();
    await new Promise((resolve) => setTimeout(resolve, 0));
    item(t, 'Leave voice').click?.();
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(r.commands).toEqual(['deafen true', 'leave']);
  });
});

describe('losing the tray host (a fake bus)', () => {
  it('shows a hidden window within one check, and revokes the confirmation', async () => {
    const r = rig();
    const c = new TrayController(r.deps);
    await c.sync();
    expect(c.working()).toBe(true);
    r.state.hidden = true;
    r.state.bus = false;
    r.tick();
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(r.shown).toBe(1);
    expect(r.revoked).toBe(1);
    expect(c.working()).toBe(false);
    expect(r.state.hidden).toBe(false);
  });

  it('does not show a window that is already visible', async () => {
    const r = rig();
    const c = new TrayController(r.deps);
    await c.sync();
    r.state.bus = false;
    await c.check();
    expect(r.shown).toBe(0);
    expect(r.revoked).toBe(1);
  });

  it('a check after the tray is turned off does nothing', async () => {
    const r = rig();
    const c = new TrayController(r.deps);
    await c.sync();
    r.settings = { ...r.settings, closeToTray: false };
    await c.sync();
    r.state.bus = false;
    r.state.hidden = true;
    await c.check();
    expect(r.shown).toBe(0);
  });
});
