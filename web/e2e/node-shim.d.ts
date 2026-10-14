// The little of Node that the browser suite uses. @types/node is not a
// dependency of web/ (a new one needs Nick's sign-off, scripts/web-depgate.sh),
// so the few functions the setup and teardown call are declared here, and no
// more. If @types/node ever arrives, delete this file.

declare module 'node:fs' {
  export function existsSync(path: string): boolean;
  export function mkdtempSync(prefix: string): string;
  export function readFileSync(path: string, encoding: 'utf8'): string;
  export function rmSync(path: string, options: { recursive: boolean; force: boolean }): void;
  export function openSync(path: string, flags: 'a'): number;
  export function closeSync(fd: number): void;
}

declare module 'node:os' {
  export function tmpdir(): string;
}

declare module 'node:path' {
  export function join(...parts: string[]): string;
  export function resolve(...parts: string[]): string;
}

declare module 'node:child_process' {
  export interface ChildProcess {
    readonly pid?: number;
    readonly exitCode: number | null;
    kill(signal?: 'SIGTERM' | 'SIGKILL'): boolean;
    unref(): void;
    once(event: 'exit', listener: () => void): ChildProcess;
  }
  export interface SpawnOptions {
    cwd?: string;
    env?: Record<string, string | undefined>;
    stdio?: 'ignore' | ('ignore' | number)[];
    detached?: boolean;
  }
  export interface SpawnSyncResult {
    readonly status: number | null;
    readonly stdout: string;
    readonly stderr: string;
    readonly error?: Error;
  }
  export function spawn(command: string, args: string[], options?: SpawnOptions): ChildProcess;
  export function spawnSync(
    command: string,
    args: string[],
    options: { cwd?: string; env?: Record<string, string | undefined>; encoding: 'utf8'; timeout?: number },
  ): SpawnSyncResult;
}

declare const process: {
  readonly env: Record<string, string | undefined>;
  kill(pid: number, signal: 'SIGTERM' | 'SIGKILL' | 0): true;
};

interface ImportMeta {
  readonly dirname: string;
}
