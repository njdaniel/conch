// The wire types of pkg/schema/run.go, generated into generated.ts, and
// runEnded, a helper of this client's that is not a wire shape.
import type { RunState } from './generated';

export { listRunsResponseV1, runState, runV1, runV1Schema as runSchema, type ListRunsResponseV1, type RunState, type RunV1 } from './generated';

// A run in one of these states has ended and never leaves it.
export function runEnded(state: RunState): boolean {
  return state === 'completed' || state === 'failed' || state === 'cancelled';
}
