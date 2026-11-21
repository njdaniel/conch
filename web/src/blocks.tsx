// Personal blocks (issue #1832, docs/design/blocks.md). The signed-in person's
// block list, read from the server (GET /v1/blocks) and read again when the
// events socket says it changed (conch.blocks_changed.v1) or reconnects, so a
// block made on one device reaches the others without a reload.
//
// A block is a preference, not a security boundary. It collapses the blocked
// person's messages in this person's log, search and threads, keeps them out
// of this person's alerts, and mutes them locally in voice. It hides nothing
// from anyone else and is never shown to the person blocked. Approval and
// admin-action cards are decisions and are never collapsed or silenced
// (isBlockExempt); moderation notices and tombstones never come from the
// author and are never collapsed either.
import { createContext, useCallback, useContext, useEffect, useId, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';
import { blockPrincipal, listBlocks, unblockPrincipal } from './api';
import { adminActionV1PayloadSchema, approvalV1PayloadSchema, type BlockV1, type MessageV2 } from './schema/generated';

// How long after the first of a burst of signals the list is read.
export const blocksCoalesce = 250;

// isBlockExempt is the server's schema.BlockExemptPayload: a message carrying
// a decision is shown and alerted whatever the reader blocks. A removed
// message (a tombstone, by a moderator or the author) is also never collapsed:
// it holds nothing of the author's to hide, and a moderation notice must show.
export function isBlockExempt(message: Pick<MessageV2, 'payload' | 'removed'>): boolean {
  if (message.removed === true) return true;
  const schema = message.payload?.schema;
  return schema === approvalV1PayloadSchema || schema === adminActionV1PayloadSchema;
}

// isCollapsedFor says whether message is collapsed for a reader who blocks
// the ids in blocked: its author is blocked and it is not exempt.
export function isCollapsedFor(blocked: ReadonlySet<number>, message: Pick<MessageV2, 'author_id' | 'payload' | 'removed'>): boolean {
  return blocked.has(message.author_id) && !isBlockExempt(message);
}

export interface Blocks {
  // The ids blocked, for the checks above.
  readonly blocked: ReadonlySet<number>;
  // The list as the server holds it, oldest first, for Settings.
  readonly list: readonly BlockV1[];
  // Whether the list has been read since sign-in.
  readonly loaded: boolean;
  readonly block: (id: number) => Promise<boolean>;
  readonly unblock: (id: number) => Promise<boolean>;
  // signal is the events socket's word that the list may have changed.
  readonly signal: () => void;
}

const none: Blocks = {
  blocked: new Set(),
  list: [],
  loaded: false,
  block: () => Promise.resolve(false),
  unblock: () => Promise.resolve(false),
  signal: () => undefined,
};

const BlocksContext = createContext<Blocks>(none);

export function useBlocksContext(): Blocks {
  return useContext(BlocksContext);
}

export function BlocksProvider({ value, children }: { readonly value: Blocks; readonly children: ReactNode }) {
  return <BlocksContext.Provider value={value}>{children}</BlocksContext.Provider>;
}

// useBlocks holds the list for a signed-in person. A guest or a page that is
// not signed in holds none: a guest keeps no list (the server refuses it).
export function useBlocks(enabled: boolean): Blocks {
  const [list, setList] = useState<readonly BlockV1[]>([]);
  const [loaded, setLoaded] = useState(false);
  const [tick, setTick] = useState(0);
  const timer = useRef<number | null>(null);
  const reads = useRef(0);

  useEffect(() => {
    if (!enabled) {
      setList([]);
      setLoaded(false);
      return;
    }
    const mine = ++reads.current;
    void listBlocks().then((result) => {
      if (mine !== reads.current || !result.ok) return;
      setList(result.value.blocks);
      setLoaded(true);
    });
  }, [enabled, tick]);

  const signal = useCallback(() => {
    if (timer.current !== null) return;
    timer.current = window.setTimeout(() => {
      timer.current = null;
      setTick((n) => n + 1);
    }, blocksCoalesce);
  }, []);
  useEffect(
    () => () => {
      if (timer.current !== null) window.clearTimeout(timer.current);
      timer.current = null;
    },
    [],
  );

  const block = useCallback(async (id: number) => {
    const result = await blockPrincipal(id);
    if (!result.ok) return false;
    const entry = result.value;
    setList((now) => (now.some((b) => b.principal_id === id) ? now : [...now, entry]));
    return true;
  }, []);
  const unblock = useCallback(async (id: number) => {
    const result = await unblockPrincipal(id);
    if (!result.ok) return false;
    setList((now) => now.filter((b) => b.principal_id !== id));
    return true;
  }, []);

  const blocked = useMemo(() => new Set(list.map((b) => b.principal_id)), [list]);
  return useMemo(() => ({ blocked, list, loaded, block, unblock, signal }), [blocked, list, loaded, block, unblock, signal]);
}

// The words every confirmation uses: what blocking does and does not do.
export const blockExplanation =
  'Their messages collapse to “Blocked message” for you, they no longer alert you, and they are muted for you in voice. ' +
  'Blocking does not remove them from anything or stop them reading what you write in a channel you share, and they are not told.';

// BlockedRow is a collapsed message: one row, with Show, which reveals it for
// this view only (nothing is stored).
export function BlockedRow({ onShow }: { readonly onShow: () => void }) {
  return (
    <div className="blocked-message" role="note">
      <span className="blocked-message-label">Blocked message</span>
      <button type="button" className="link-button blocked-message-show" onClick={onShow}>
        Show
      </button>
    </div>
  );
}

// BlockConfirm asks before a block, saying what it does and does not do, with
// focus on Cancel. Escape and Cancel close it; so does a block that is made.
export function BlockConfirm({ id, name, onClose }: { readonly id: number; readonly name: string; readonly onClose: () => void }) {
  const { block } = useBlocksContext();
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState(false);
  const cancel = useRef<HTMLButtonElement>(null);
  const questionId = useId();
  useEffect(() => {
    cancel.current?.focus();
  }, []);
  function leave(event: KeyboardEvent<HTMLDivElement>) {
    if (event.key !== 'Escape' || event.nativeEvent.isComposing) return;
    event.preventDefault();
    event.stopPropagation();
    onClose();
  }
  return (
    <div role="group" aria-labelledby={questionId} className="member-confirm block-confirm" onKeyDown={leave}>
      <p id={questionId}>{`Block ${name}? ${blockExplanation}`}</p>
      {failed ? <p role="alert">{`Could not block ${name}. Try again.`}</p> : null}
      <div className="actions">
        <button
          type="button"
          disabled={busy}
          onClick={() => {
            setBusy(true);
            setFailed(false);
            void block(id).then((ok) => {
              setBusy(false);
              if (ok) onClose();
              else setFailed(true);
            });
          }}
        >
          Yes, block
        </button>
        <button ref={cancel} type="button" onClick={onClose}>
          Cancel
        </button>
      </div>
    </div>
  );
}

// BlockToggle is the profile card's control: Block… (which asks first) or
// Unblock. It shows nothing on the viewer's own card, or before the list is
// read (a guest's page never reads one).
export function BlockToggle({ id, name, viewer }: { readonly id: number; readonly name: string; readonly viewer?: number }) {
  const { blocked, unblock, loaded } = useBlocksContext();
  const [asking, setAsking] = useState(false);
  if (!loaded || id === viewer) return null;
  if (blocked.has(id)) {
    return (
      <button type="button" className="profile-card-block" onClick={() => void unblock(id)}>
        Unblock
      </button>
    );
  }
  if (asking) return <BlockConfirm id={id} name={name} onClose={() => { setAsking(false); }} />;
  return (
    <button type="button" className="profile-card-block" onClick={() => { setAsking(true); }}>
      Block…
    </button>
  );
}

// BlockedPeople is Settings' list of the people this person blocks, oldest
// first, each with Unblock. It is read from the server, so every device shows
// the same list.
export function BlockedPeople() {
  const { list, loaded, unblock } = useBlocksContext();
  const [failed, setFailed] = useState<string | null>(null);
  if (!loaded) return null;
  return (
    <div className="blocked-people">
      <h3>Blocked people</h3>
      <p className="hint">{blockExplanation}</p>
      {list.length === 0 ? (
        <p>You have not blocked anyone.</p>
      ) : (
        <ul>
          {list.map((b) => (
            <li key={b.principal_id}>
              <span>{b.kind === 'agent' ? `${b.name} (agent)` : b.name}</span>{' '}
              <button
                type="button"
                onClick={() => {
                  setFailed(null);
                  void unblock(b.principal_id).then((ok) => {
                    if (!ok) setFailed(`Could not unblock ${b.name}. Try again.`);
                  });
                }}
              >
                Unblock
              </button>
            </li>
          ))}
        </ul>
      )}
      {failed === null ? null : <p role="alert">{failed}</p>}
    </div>
  );
}

// useBlockedVoiceMute turns each blocked person in the room to volume 0 on
// this page, once per person per page: the default, not a lock. Moving their
// slider (or Unmute) is the person's own choice and is kept, and nothing is
// sent anywhere: it changes how loud this page plays them, for no one else.
// Unblocking someone this muted, while they are still at 0, plays them again.
export function useBlockedVoiceMute(
  blocked: ReadonlySet<number>,
  others: readonly { readonly principal: number }[],
  volumes: ReadonlyMap<number, number>,
  setVolume: (principal: number, volume: number) => void,
): void {
  const muted = useRef(new Set<number>());
  useEffect(() => {
    for (const o of others) {
      if (blocked.has(o.principal) && !muted.current.has(o.principal)) {
        muted.current.add(o.principal);
        setVolume(o.principal, 0);
      }
    }
    for (const id of [...muted.current]) {
      if (blocked.has(id)) continue;
      muted.current.delete(id);
      if ((volumes.get(id) ?? 1) === 0) setVolume(id, 1);
    }
  }, [blocked, others, volumes, setVolume]);
}
