// The opt-in tray (issue #1589, D5-03 of #773; linux-desktop.md §9).
// Input: the app's settings, the voice service's confirmed view, the screen
// companion's state, and a detector for the session's tray host. Output: a
// tray icon and menu while (and only while) closeToTray is on, the answer to
// "is a tray working?" for the close rule, and the window shown again if the
// tray stops working while the window is hidden.
// Owns: the state-to-menu mapping (a pure function), the Tray and its menu,
// the periodic check, and the pending flag of a menu command.
//
// What it keeps:
//   - the menu shows only what the helper has confirmed. A command in flight
//     disables the voice actions; it never changes a label ("Muting...").
//     A command that fails leaves the labels alone and logs the reason code
//     alone;
//   - a tray is working only when closeToTray is on, the person has confirmed
//     they can see its icon, and the tray host is on the bus right now
//     (Q-D5-2). Losing the host revokes the confirmation, and shows the
//     window if it was hidden: there is never an invisible running session;
//   - no profile name, channel or state is logged. The menu is rebuilt from
//     the activation in force at each refresh, and blanked first when media
//     ends for a switch or a sign-out.
import { join } from 'node:path';

import type { NativeVoiceView, VoiceCommandResult } from '../../web/src/platform/desktop/bridge.ts';
import type { AppSettings } from './settings.ts';

export type TrayIcon = 'idle' | 'voice' | 'transmitting' | 'sharing';
export type TrayCommand = 'mute' | 'deafen' | 'leave';

export interface TrayInput {
  // The label of the profile in force, or null with none.
  readonly profile: string | null;
  // VoiceService's confirmed view.
  readonly voice: NativeVoiceView;
  // The screen companion is live (#1579).
  readonly sharing: boolean;
  // A menu command is waiting for the helper's answer.
  readonly pending: boolean;
}

export interface TrayAction {
  readonly label: string;
  readonly enabled: boolean;
}

export interface TrayModel {
  readonly icon: TrayIcon;
  readonly header: string;
  readonly voiceLine: string;
  // One of Transmitting, Muted, Deafened or Sharing your screen, or none.
  readonly stateLine: string | null;
  readonly mute: TrayAction;
  readonly deafen: TrayAction;
  readonly leave: TrayAction;
}

// trayModel maps what is confirmed to what is shown. Transmitting appears
// only when the helper's state says transmitting.
export function trayModel(input: TrayInput): TrayModel {
  const { voice, sharing, pending } = input;
  // Only a running helper's own report counts.
  const helper = voice.status === 'running' ? voice.helper : undefined;
  const running = helper !== undefined;
  const channel = running ? voice.channel : null;
  const inVoice = channel !== null && helper?.phase === 'connected';
  const transmitting = inVoice && helper.transmitting;
  const muted = helper?.muted === true;
  const deafened = helper?.deafened === true;
  let stateLine: string | null = null;
  if (sharing) stateLine = 'Sharing your screen';
  else if (transmitting) stateLine = 'Transmitting';
  else if (deafened) stateLine = 'Deafened';
  else if (muted) stateLine = 'Muted';
  const icon: TrayIcon = sharing ? 'sharing' : transmitting ? 'transmitting' : inVoice ? 'voice' : 'idle';
  // The actions are offered while voice runs, and not while a command waits.
  const enabled = running && !pending;
  return {
    icon,
    header: input.profile ?? 'Conch',
    voiceLine: inVoice ? `In voice: #${channel}` : 'Not in voice',
    stateLine,
    mute: { label: muted ? 'Unmute' : 'Mute', enabled },
    deafen: { label: deafened ? 'Undeafen' : 'Deafen', enabled },
    leave: { label: 'Leave voice', enabled },
  };
}

export interface TrayMenuItem {
  readonly label?: string;
  readonly type?: 'separator';
  readonly enabled?: boolean;
  readonly click?: () => void;
}

// What the controller uses of Electron's Tray and Menu; a test stands in.
export interface TrayLike {
  setImage(path: string): void;
  setToolTip(text: string): void;
  setContextMenu(menu: unknown): void;
  on(event: 'click', listener: () => void): void;
  destroy(): void;
}

export interface TrayDeps {
  readonly createTray: (iconPath: string) => TrayLike;
  readonly buildMenu: (items: readonly TrayMenuItem[]) => unknown;
  // The directory the icons are in.
  readonly iconDir: string;
  readonly voice: {
    state(): NativeVoiceView;
    subscribe(listener: (view: NativeVoiceView) => void): () => void;
    setMute(muted: boolean): Promise<VoiceCommandResult>;
    setDeafen(deafened: boolean): Promise<VoiceCommandResult>;
    leave(): Promise<NativeVoiceView>;
  };
  // Whether the screen companion is live.
  readonly sharing: () => boolean;
  // The label of the profile in force.
  readonly profile: () => string | null;
  // Show or focus the window, making one visible again if it was hidden.
  readonly show: () => void;
  // Whether the window is hidden by a close.
  readonly hidden: () => boolean;
  // The full quit: before-quit's teardown runs as for the window's own.
  readonly quit: () => void;
  // Is the tray host on the bus.
  readonly detect: () => Promise<boolean>;
  // The person's confirmation is revoked (the host went away).
  readonly revoke: () => void;
  readonly settings: () => AppSettings;
  readonly confirmed: () => boolean;
  // A repeating timer; returns its stop. A test supplies its own.
  readonly every?: (fn: () => void, ms: number) => () => void;
  readonly log: (line: string) => void;
}

export const CHECK_INTERVAL_MS = 10_000;

export function iconFile(icon: TrayIcon): string {
  return `${icon}.png`;
}

export class TrayController {
  readonly #deps: TrayDeps;
  #tray: TrayLike | null = null;
  #unsubscribe: (() => void) | null = null;
  #stopTimer: (() => void) | null = null;
  // The last answer of the detector; false until the first check.
  #host = false;
  #pending = false;
  // Blank until the next refresh: set when media ends for a switch.
  #blank = false;

  constructor(deps: TrayDeps) {
    this.#deps = deps;
  }

  get exists(): boolean {
    return this.#tray !== null;
  }

  // working: the close rule's answer, synchronous and conservative.
  working(): boolean {
    return this.#tray !== null && this.#deps.settings().closeToTray && this.#deps.confirmed() && this.#host;
  }

  // sync makes the tray exist exactly while closeToTray is on.
  async sync(): Promise<void> {
    if (!this.#deps.settings().closeToTray) {
      this.#destroy();
      return;
    }
    if (this.#tray === null) {
      const model = this.#model();
      const tray = this.#deps.createTray(join(this.#deps.iconDir, iconFile(model.icon)));
      tray.setToolTip('Conch');
      tray.on('click', () => {
        this.#deps.show();
      });
      this.#tray = tray;
      this.#unsubscribe = this.#deps.voice.subscribe(() => {
        this.refresh();
      });
      const every =
        this.#deps.every ??
        ((fn, ms) => {
          const handle = setInterval(fn, ms);
          return () => {
            clearInterval(handle);
          };
        });
      this.#stopTimer = every(() => {
        void this.check();
      }, CHECK_INTERVAL_MS);
    }
    this.refresh();
    await this.check();
  }

  // check asks the bus. Losing the host revokes the confirmation and, if the
  // window is hidden, shows it: the fail-safe.
  async check(): Promise<void> {
    if (this.#tray === null) return;
    const present = await this.#deps.detect();
    // The tray may have been turned off while the bus was asked.
    if (!this.exists) return;
    this.#host = present;
    if (!present) {
      if (this.#deps.confirmed()) this.#deps.revoke();
      if (this.#deps.hidden()) {
        this.#deps.log('tray: the tray host is gone; showing the window');
        this.#deps.show();
      }
    }
    this.refresh();
  }

  // blank empties the menu of the profile and voice (media is ending for a
  // switch or sign-out); the next refresh fills it from the new activation.
  blank(): void {
    this.#blank = true;
    this.refresh();
  }

  refresh(): void {
    const tray = this.#tray;
    if (tray === null) return;
    const model = this.#model();
    tray.setImage(join(this.#deps.iconDir, iconFile(model.icon)));
    tray.setContextMenu(this.#deps.buildMenu(this.#items(model)));
  }

  #model(): TrayModel {
    const voice = this.#deps.voice.state();
    if (this.#blank) {
      this.#blank = false;
      return trayModel({ profile: null, voice: { status: 'idle', generation: voice.generation, channel: null, retryable: false }, sharing: false, pending: false });
    }
    return trayModel({ profile: this.#deps.profile(), voice, sharing: this.#deps.sharing(), pending: this.#pending });
  }

  #items(model: TrayModel): TrayMenuItem[] {
    const items: TrayMenuItem[] = [{ label: model.header, enabled: false }, { label: model.voiceLine, enabled: false }];
    if (model.stateLine !== null) items.push({ label: model.stateLine, enabled: false });
    items.push(
      { type: 'separator' },
      {
        label: 'Show Conch',
        click: () => {
          this.#deps.show();
        },
      },
      this.#command(model.mute, 'mute', () => this.#deps.voice.setMute(!this.#deps.voice.state().helper?.muted)),
      this.#command(model.deafen, 'deafen', () => this.#deps.voice.setDeafen(!this.#deps.voice.state().helper?.deafened)),
      this.#command(model.leave, 'leave', () => this.#deps.voice.leave().then(() => ({ ok: true as const }))),
      { type: 'separator' },
      {
        label: 'Quit',
        click: () => {
          this.#deps.quit();
        },
      },
    );
    return items;
  }

  #command(action: TrayAction, name: TrayCommand, run: () => Promise<VoiceCommandResult>): TrayMenuItem {
    return {
      label: action.label,
      enabled: action.enabled,
      click: () => {
        void this.#run(name, run);
      },
    };
  }

  // The label changes only on the helper's next state, which refreshes the
  // menu; a failure is logged by its reason code alone.
  async #run(name: TrayCommand, run: () => Promise<VoiceCommandResult>): Promise<void> {
    if (this.#pending) return;
    this.#pending = true;
    this.refresh();
    try {
      const result = await run();
      if (!result.ok) this.#deps.log(`tray: ${name} failed (${result.error})`);
    } catch {
      this.#deps.log(`tray: ${name} failed`);
    } finally {
      this.#pending = false;
      this.refresh();
    }
  }

  #destroy(): void {
    this.#unsubscribe?.();
    this.#unsubscribe = null;
    this.#stopTimer?.();
    this.#stopTimer = null;
    this.#tray?.destroy();
    this.#tray = null;
    this.#host = false;
  }
}
