// Windows F16 (issue #905; windows-desktop.md §8). Input: a main-owned
// window/process snapshot and the current playback pids. Output: the root
// pid eligible for include-tree capture, or a picture-only refusal with fixed
// words. Owns no process and starts no capture. The Windows picker/capture
// adapter is #778's: it must use this root, never endpoint loopback, and ask
// again for each pick. A renderer must never supply these pids or snapshots.

export interface AudioProcess {
  readonly pid: number;
  readonly parentPid: number;
}

export interface WindowProcessSnapshot {
  // The owner of the picked window, resolved by the native adapter, not its
  // title (and not Electron's source id interpreted as a pid).
  readonly windowPid: number;
  // Native-verified boundary of exactly the selected application, resolved in
  // the same read. null means the adapter cannot prove that include-tree
  // capture stays within that application. Parent links, executable names,
  // an exited parent or a shared launcher (Explorer, Steam, etc.) alone are
  // not boundary evidence. The adapter must return null without such proof.
  readonly applicationRootPid: number | null;
  // A complete process snapshot, from the same read as the window owner.
  // An absent parent denotes a process whose launcher has already exited.
  readonly processes: readonly AudioProcess[];
}

export interface PlaybackProcesses {
  readonly shellPid: number;
  // null means no helper is running, not a failure to determine its pid.
  readonly helperPid: number | null;
}

export const APPLICATION_AUDIO_REASONS = {
  'contains-call': 'Sharing the picture only: sound from this application would include the call.',
  unavailable: 'Sharing the picture only: sound from this application is not available here.',
} as const;

export type ApplicationAudioDecision =
  | { readonly allowed: true; readonly rootPid: number }
  | { readonly allowed: false; readonly reason: keyof typeof APPLICATION_AUDIO_REASONS; readonly message: string };

function refuse(reason: keyof typeof APPLICATION_AUDIO_REASONS): ApplicationAudioDecision {
  return { allowed: false, reason, message: APPLICATION_AUDIO_REASONS[reason] };
}

function pid(value: number): boolean {
  return Number.isSafeInteger(value) && value > 0 && value <= 0xffff_ffff;
}

// Parent links validate the native boundary and test subtree membership;
// they never establish an application boundary themselves. Cycles, duplicate
// pids and incomplete identities fail closed.
export function applicationAudioDecision(snapshot: WindowProcessSnapshot, playback: PlaybackProcesses): ApplicationAudioDecision {
  if (snapshot.applicationRootPid === null || !pid(snapshot.applicationRootPid) || !pid(snapshot.windowPid) || !pid(playback.shellPid) || (playback.helperPid !== null && !pid(playback.helperPid))) return refuse('unavailable');
  const processes = new Map<number, AudioProcess>();
  for (const process of snapshot.processes) {
    // Windows' system-wide snapshot can include the idle process (0).
    // It is the parent sentinel, never an application capture target.
    if (process.pid === 0 && process.parentPid === 0) continue;
    if (!pid(process.pid) || !(process.parentPid === 0 || pid(process.parentPid)) || processes.has(process.pid)) return refuse('unavailable');
    processes.set(process.pid, process);
  }
  const ancestry = (start: number): Set<number> | null => {
    let current = processes.get(start);
    if (current === undefined) return null;
    const seen = new Set<number>();
    for (;;) {
      if (seen.has(current.pid)) return null;
      seen.add(current.pid);
      const parent = processes.get(current.parentPid);
      if (parent === undefined) return seen;
      current = parent;
    }
  };
  const selected = ancestry(snapshot.windowPid);
  const shell = ancestry(playback.shellPid);
  const helper = playback.helperPid === null ? null : ancestry(playback.helperPid);
  if (selected === null || shell === null || (playback.helperPid !== null && helper === null)
    || !selected.has(snapshot.applicationRootPid)) return refuse('unavailable');
  if (shell.has(snapshot.applicationRootPid) || helper?.has(snapshot.applicationRootPid)) return refuse('contains-call');
  return { allowed: true, rootPid: snapshot.applicationRootPid };
}

export interface ApplicationAudioDeps {
  // Native, main-owned adapter. null/rejection includes a closed window,
  // inaccessible processes or an unsupported API. No title matching.
  readonly resolve: (sourceId: string) => Promise<WindowProcessSnapshot | null>;
  readonly playback: () => PlaybackProcesses;
}

export async function resolveApplicationAudio(sourceId: string, deps: ApplicationAudioDeps): Promise<ApplicationAudioDecision> {
  try {
    const { shellPid, helperPid } = deps.playback();
    const snapshot = await deps.resolve(sourceId);
    const after = deps.playback();
    // A helper restart while the OS query was pending makes the snapshot
    // stale. Keep the picture; a later pick can resolve it afresh.
    if (snapshot === null || shellPid !== after.shellPid || helperPid !== after.helperPid) return refuse('unavailable');
    return applicationAudioDecision(snapshot, after);
  } catch {
    return refuse('unavailable');
  }
}
