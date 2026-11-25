// An isolated Secret Service for the smoke tests (issue #2077): a private
// session bus and a gnome-keyring of its own, in a throwaway directory, so the
// packaged app's safeStorage meets a real keyring without the developer's own.
//
// The bus has NO service directories, so nothing on it is ever started by
// activation: no portal, no keyring from the developer's data directory, no
// prompter. With `keyring: 'unlocked'` a gnome-keyring is started on it with a
// fresh login keyring (its data directory is the throwaway one) and unlocked
// with a fixed test password; with `keyring: 'none'` the bus has no Secret
// Service at all, which is what a desktop without a keyring looks like to
// Chromium. Nothing outside the throwaway directories is read or written.
//
// The bus socket lives in a short private directory under /tmp, because a
// Unix socket path is limited to 108 bytes and a test's tmpdir may be long.
import { spawn, spawnSync, type ChildProcess } from 'node:child_process';
import { chmodSync, existsSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

export interface SecretBus {
  // The environment the app is launched with: the private bus, the throwaway
  // XDG directories and a desktop name Chromium maps to libsecret.
  readonly env: Readonly<Record<string, string>>;
  readonly stop: () => Promise<void>;
}

function onPath(command: string): boolean {
  return spawnSync('sh', ['-c', `command -v ${command}`], { stdio: 'ignore' }).status === 0;
}

// Why an isolated Secret Service cannot be made here, or null when it can.
export function secretBusMissing(): string | null {
  const missing = ['dbus-daemon', 'gnome-keyring-daemon', 'busctl'].filter((c) => !onPath(c));
  return missing.length === 0 ? null : `an isolated Secret Service needs ${missing.join(', ')} on PATH`;
}

function waitFor(check: () => boolean, ms: number, what: string): Promise<void> {
  const deadline = Date.now() + ms;
  return new Promise((resolve, reject) => {
    const tick = (): void => {
      if (check()) {
        resolve();
        return;
      }
      if (Date.now() > deadline) {
        reject(new Error(`${what} within ${String(ms)} ms`));
        return;
      }
      setTimeout(tick, 100);
    };
    tick();
  });
}

function stopChild(child: ChildProcess | null): Promise<void> {
  if (child === null || child.exitCode !== null || child.signalCode !== null) return Promise.resolve();
  return new Promise((resolve) => {
    const timer = setTimeout(() => child.kill('SIGKILL'), 3_000);
    child.once('exit', () => {
      clearTimeout(timer);
      resolve();
    });
    child.kill('SIGTERM');
  });
}

export async function startSecretBus(options: { readonly keyring: 'unlocked' | 'none' }): Promise<SecretBus> {
  const home = mkdtempSync(join(tmpdir(), 'conch-desktop-keyring-'));
  const sock = mkdtempSync(join(existsSync('/tmp') ? '/tmp' : tmpdir(), 'cdk-'));
  chmodSync(sock, 0o700);
  const xdg = {
    XDG_DATA_HOME: join(home, 'data'),
    XDG_CONFIG_HOME: join(home, 'config'),
    XDG_CACHE_HOME: join(home, 'cache'),
  };
  for (const dir of Object.values(xdg)) mkdirSync(dir, { recursive: true, mode: 0o700 });
  const control = join(home, 'control');
  mkdirSync(control, { mode: 0o700 });
  const config = join(home, 'session.conf');
  writeFileSync(
    config,
    `<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-Bus Bus Configuration 1.0//EN" "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
<busconfig>
  <type>session</type>
  <listen>unix:dir=${sock}</listen>
  <auth>EXTERNAL</auth>
  <policy context="default">
    <allow send_destination="*" eavesdrop="true"/>
    <allow eavesdrop="true"/>
    <allow own="*"/>
  </policy>
</busconfig>
`,
  );
  // The bus daemon gets only the throwaway directories, never the caller's
  // own, so that nothing it could start reads the developer's keyring.
  const base = { PATH: process.env['PATH'] ?? '/usr/bin:/bin', HOME: home, ...xdg };
  const bus = spawn('dbus-daemon', ['--config-file', config, '--nofork', '--print-address=1'], { env: base, stdio: ['ignore', 'pipe', 'ignore'] });
  let address = '';
  bus.stdout.on('data', (chunk: Buffer) => {
    address += chunk.toString('utf8');
  });
  let keyring: ChildProcess | null = null;
  const stop = async (): Promise<void> => {
    await stopChild(keyring);
    await stopChild(bus);
    rmSync(home, { recursive: true, force: true });
    rmSync(sock, { recursive: true, force: true });
  };
  try {
    await waitFor(() => address.includes('\n'), 10_000, 'the private bus did not start');
    const busAddress = address.trim();
    const env = { ...base, DBUS_SESSION_BUS_ADDRESS: busAddress };
    if (options.keyring === 'unlocked') {
      // --unlock reads the login keyring's password on stdin and makes that
      // keyring, unlocked, when there is none yet.
      keyring = spawn('gnome-keyring-daemon', ['--foreground', '--unlock', '--components=secrets', `--control-directory=${control}`], { env, stdio: ['pipe', 'ignore', 'ignore'] });
      keyring.stdin?.end('conch-smoke-keyring');
      await waitFor(
        () => spawnSync('busctl', ['--user', 'status', 'org.freedesktop.secrets'], { env, stdio: 'ignore' }).status === 0,
        10_000,
        'the isolated Secret Service did not appear on the bus',
      );
    }
    // GNOME: a desktop name Chromium maps to its libsecret backend, as it does
    // COSMIC (measured with Electron 44 for #2077).
    return { env: { ...env, XDG_CURRENT_DESKTOP: 'GNOME' }, stop };
  } catch (err) {
    await stop();
    throw err;
  }
}
