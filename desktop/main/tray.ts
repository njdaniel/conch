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
//     window if it was hidden: there is never an invisible running session.
//     With the window hidden, losing the host also mutes any helper (whatever
//     its status or muted flag), leaves voice if the mute is refused or times
//     out, and ends a live screen companion: not being able to show an
//     indicator is not permission to keep transmitting (§9's lock rule). Destroying the tray (the setting turned
//     off) with the window hidden shows the window too;
//   - Mute and Deafen send the intent bound when the menu was built (the
//     model's muted and deafened), never a toggle against live state, so a
//     click on a stale menu cannot do the opposite of its label;
//   - no profile name, channel or state is logged. The menu is rebuilt from
//     the activation in force at each refresh, and blanked when media ends
//     for a switch or a sign-out, staying blank until activated() says the
//     new activation is open.
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
  // The helper's confirmed flags when the model was built: the menu's Mute
  // and Deafen send the opposite of these, bound at build time.
  readonly muted: boolean;
  readonly deafened: boolean;
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
    muted,
    deafened,
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
  // End the live screen companion (app.ts passes companion.end('lock'): the
  // existing reason for "the session can no longer show its indicator";
  // adding a 'tray' reason would reach the bridge, preload and page types).
  readonly endCompanion: () => Promise<boolean>;
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

// unblankSoon: an activation was opened; the menu is filled from it on the
// next turn, once the switcher has recorded it. Only if its generation is
// still current: with a second switch queued behind it, this activation is
// already on its way out and must not unblank the menu with the outgoing
// profile (the next activation's own call does it). The tray is read when the
// turn comes, not now: it may be made after the first activation opens.
export function unblankSoon(tray: () => Pick<TrayController, 'activated'> | null, isCurrent: (generation: number) => boolean, generation: number): void {
  setTimeout(() => {
    if (isCurrent(generation)) tray()?.activated();
  }, 0);
}

export class TrayController {
  readonly #deps: TrayDeps;
  #tray: TrayLike | null = null;
  #unsubscribe: (() => void) | null = null;
  #stopTimer: (() => void) | null = null;
  // The last answer of the detector; false until the first check.
  #host = false;
  #pending = false;
  // Blank until activated(): set when media ends for a switch or a sign-out,
  // and kept through every refresh in between (a refresh from the helper's
  // teardown or the timer must not bring the old profile back).
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
      let tray: TrayLike | null = null;
      try {
        tray = this.#deps.createTray(join(this.#deps.iconDir, iconFile(model.icon)));
        tray.setToolTip('Conch');
        tray.on('click', () => {
          this.#deps.show();
        });
      } catch {
        // No tray could be made: none exists, so closing quits (§9).
        this.#deps.log('tray: the tray could not be created');
        try {
          tray?.destroy();
        } catch {
          // Nothing more to undo.
        }
        return;
      }
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
        // check() does not reject; the catch keeps a surprise from becoming an
        // unhandled rejection that would end the timer's work silently.
        this.check().catch(() => {
          this.#deps.log('tray: the check failed');
        });
      }, CHECK_INTERVAL_MS);
    }
    this.refresh();
    await this.check();
  }

  // check asks the bus. Losing the host revokes the confirmation and, if the
  // window is hidden, shows it: the fail-safe.
  async check(): Promise<void> {
    if (this.#tray === null) return;
    let present: boolean;
    try {
      present = await this.#deps.detect();
    } catch {
      // A probe that cannot answer is no host: fail closed.
      present = false;
    }
    // The tray may have been turned off while the bus was asked.
    if (!this.exists) return;
    this.#host = present;
    if (!present) {
      if (this.#deps.confirmed()) this.#deps.revoke();
      if (this.#deps.hidden()) {
        this.#deps.log('tray: the tray host is gone; showing the window');
        // Showing is best effort: the window failing to show must not skip
        // the mute below.
        try {
          this.#deps.show();
        } catch {
          this.#deps.log('tray: the window could not be shown');
        }
        this.#failSafe();
      }
    }
    this.refresh();
  }

  // failSafe: the window was hidden and its indicator is gone, so nothing may
  // keep transmitting or sharing unseen. A helper (in any status that has a
  // process: starting, running, stopping) is muted, whatever its muted flag
  // says (the flag is only what the helper last reported, and an unmute may be
  // in flight); a mute that is not ok, or is not-running (no helper after all),
  // falls back to leaving voice. A live screen companion is ended. Best
  // effort; failures are logged by their reason alone.
  #failSafe(): void {
    const view = this.#deps.voice.state();
    if (view.status === 'starting' || view.status === 'running' || view.status === 'stopping' || view.helper !== undefined) {
      this.#deps.log('tray: muting, as the tray host is gone');
      this.#deps.voice.setMute(true).then(
        (result) => {
          if (result.ok || result.error === 'not-running') return;
          this.#deps.log(`tray: mute failed (${result.error}); leaving voice`);
          this.#leave();
        },
        () => {
          this.#deps.log('tray: mute failed; leaving voice');
          this.#leave();
        },
      );
    }
    if (this.#deps.sharing()) {
      this.#deps.log('tray: ending the screen share, as the tray host is gone');
      this.#deps.endCompanion().catch(() => {
        this.#deps.log('tray: ending the screen share failed');
      });
    }
  }

  #leave(): void {
    this.#deps.voice.leave().catch(() => {
      this.#deps.log('tray: leaving voice failed');
    });
  }

  // blank empties the menu of the profile and voice (media is ending for a
  // switch or sign-out). It stays empty until activated().
  blank(): void {
    this.#blank = true;
    this.refresh();
  }

  // activated: the new activation is open; the menu is filled from it now.
  activated(): void {
    this.#blank = false;
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
      this.#command(model.mute, 'mute', () => this.#deps.voice.setMute(!model.muted)),
      this.#command(model.deafen, 'deafen', () => this.#deps.voice.setDeafen(!model.deafened)),
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
    const had = this.#tray !== null;
    try {
      this.#tray?.destroy();
    } catch {
      // A tray that cannot be destroyed is gone from our side all the same.
    }
    this.#tray = null;
    this.#host = false;
    // With the icon gone nothing can bring a hidden window back.
    if (had && this.#deps.hidden()) {
      this.#deps.log('tray: the tray is off; showing the window');
      this.#deps.show();
    }
  }
}
