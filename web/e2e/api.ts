// What the tests read from the server directly, as a person's credential sees
// it. The shapes are the web client's own schema modules, which are checked
// against pkg/schema's golden fixtures.
import { listActivityResponseV1, type AgentActivityV1 } from '../src/schema/activity';
import { listInterventionsResponseV1, type InterventionV1 } from '../src/schema/intervention';
import { listMessagesResponseV2, postMessageResponseV2, type MessageV2 } from '../src/schema/message';
import { listReadStateResponseV1, type ReadStateV1 } from '../src/schema/readstate';
import { listRunsResponseV1, runV1, type RunV1 } from '../src/schema/run';
import { sessionRosterV1, sessionV1, type SessionV1 } from '../src/schema/session';
import { call, ok, type World } from './world';

// history is every message of a channel, oldest first.
export async function history(world: World, token: string, channel: string): Promise<MessageV2[]> {
  const all: MessageV2[] = [];
  let after = 0;
  for (let page = 0; page < 50; page++) {
    const path = `/v2/channels/${encodeURIComponent(channel)}/messages?after=${String(after)}&limit=100`;
    const list = await ok(world, token, 'GET', path, listMessagesResponseV2);
    all.push(...list.messages);
    const next = list.next_after ?? 0;
    if (next <= after) break;
    after = next;
  }
  return all;
}

export async function post(world: World, token: string, channel: string, body: string): Promise<MessageV2> {
  const reply = await ok(world, token, 'POST', `/v2/channels/${encodeURIComponent(channel)}/messages`, postMessageResponseV2, { body });
  return reply.message;
}

export async function roster(world: World, token: string, channel: string): Promise<SessionV1[]> {
  return (await ok(world, token, 'GET', `/v1/sessions?channel=${encodeURIComponent(channel)}`, sessionRosterV1)).sessions;
}

export function openSession(world: World, token: string, client: string): Promise<SessionV1> {
  return ok(world, token, 'POST', '/v1/sessions', sessionV1, { client });
}

export async function heartbeat(world: World, token: string, session: number): Promise<void> {
  const reply = await call(world, token, 'POST', `/v1/sessions/${String(session)}/heartbeat`);
  if (reply.status < 200 || reply.status > 299) throw new Error(`heartbeat of session ${String(session)}: status ${String(reply.status)}`);
}

export async function runsIn(world: World, token: string, channel: string): Promise<RunV1[]> {
  return (await ok(world, token, 'GET', `/v1/runs?channel=${encodeURIComponent(channel)}&order=desc&limit=20`, listRunsResponseV1)).runs;
}

export function run(world: World, token: string, id: number): Promise<RunV1> {
  return ok(world, token, 'GET', `/v1/runs/${String(id)}`, runV1);
}

export async function interventions(world: World, token: string, runId: number): Promise<InterventionV1[]> {
  return (await ok(world, token, 'GET', `/v1/runs/${String(runId)}/interventions`, listInterventionsResponseV1)).interventions;
}

export async function activityOf(world: World, token: string, runId: number): Promise<AgentActivityV1[]> {
  return (await ok(world, token, 'GET', `/v1/runs/${String(runId)}/activity?after=0&limit=100`, listActivityResponseV1)).activity;
}

// readState is the caller's mark and unread count in every channel they are in.
export async function readState(world: World, token: string): Promise<ReadStateV1[]> {
  return (await ok(world, token, 'GET', '/v1/read-state', listReadStateResponseV1)).read_state;
}
