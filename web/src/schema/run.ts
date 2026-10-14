// Copies of pkg/schema/run.go.
import { array, int, object, optional, str, timestamp, vocab } from './validate';

export type RunState = 'queued' | 'starting' | 'running' | 'blocked' | 'waiting' | 'completed' | 'failed' | 'cancelled';

export const runState = vocab<RunState>({
  queued: true,
  starting: true,
  running: true,
  blocked: true,
  waiting: true,
  completed: true,
  failed: true,
  cancelled: true,
});

// A run in one of these states has ended and never leaves it.
export function runEnded(state: RunState): boolean {
  return state === 'completed' || state === 'failed' || state === 'cancelled';
}

export const runSchema = 'conch.run.v1';

// RunV1 is one unit of work an agent session reports. link, when present, is
// an https URL by the server's rule; the page checks that again before it
// makes a link of it.
export interface RunV1 {
  schema: typeof runSchema;
  id: number;
  session_id: number;
  principal_id: number;
  channel_id: number;
  message_id?: number;
  state: RunState;
  task: string;
  link?: string;
  started_at: string;
  ended_at?: string;
  outcome?: string;
}

export const runV1 = object<RunV1>({
  schema: vocab<typeof runSchema>({ [runSchema]: true }),
  id: int,
  session_id: int,
  principal_id: int,
  channel_id: int,
  message_id: optional(int),
  state: runState,
  task: str,
  link: optional(str),
  started_at: timestamp,
  ended_at: optional(timestamp),
  outcome: optional(str),
});

export interface ListRunsResponseV1 {
  runs: RunV1[];
  next_after?: number;
}

export const listRunsResponseV1 = object<ListRunsResponseV1>({
  runs: array(runV1),
  next_after: optional(int),
});
