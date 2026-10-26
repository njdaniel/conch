import { useCallback, useEffect, useRef, useState } from 'react';
import * as api from './api';
import { Message } from './Conversation';
import type { MessageV2 } from './schema/message';
import type { LiveStatus, Person } from './useLiveChannel';

interface Context {
  readonly id: number;
  readonly messages: readonly MessageV2[];
  readonly error: boolean;
}

export function SearchPanel({ channelId, channel, live, people, nets, viewer, onClose, onMissing, onUnauthorized }: {
  readonly channelId: number;
  readonly channel: string;
  readonly live: LiveStatus;
  readonly people: ReadonlyMap<number, Person>;
  readonly nets: ReadonlyMap<number, string>;
  readonly viewer: number;
  readonly onClose: () => void;
  readonly onMissing: (channel: number) => void;
  readonly onUnauthorized?: () => void;
}) {
  const [draft, setDraft] = useState('');
  const [query, setQuery] = useState('');
  const [results, setResults] = useState<readonly MessageV2[]>([]);
  const [nextBefore, setNextBefore] = useState<number | undefined>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(false);
  const [context, setContext] = useState<Context | null>(null);
  const request = useRef(0);
  const contextRequest = useRef(0);
  const previousLive = useRef(live);
  const resultView = useRef<HTMLDivElement>(null);
  const contextView = useRef<HTMLDivElement>(null);
  const returnTo = useRef<number | null>(null);
  const closeContext = useCallback(() => {
    contextRequest.current++;
    setContext(null);
  }, []);

  const attempted = useRef('');
  const search = useCallback(async (words: string, before?: number) => {
    const current = ++request.current;
    attempted.current = words;
    setBusy(true);
    setError(false);
    if (before === undefined) setNextBefore(undefined);
    const result = await api.searchMessages(channel, words, before);
    if (current !== request.current) return;
    setBusy(false);
    if (!result.ok) {
      if (result.failure === 'unauthorized') {
        setResults([]);
        setNextBefore(undefined);
        closeContext();
        onUnauthorized?.();
        return;
      }
      setError(true);
      if (before === undefined) setResults([]);
      if (result.code === 'channel_not_found') {
        setResults([]);
        setNextBefore(undefined);
        closeContext();
        onMissing(channelId);
      }
      return;
    }
    setQuery(words);
    setResults((previous) => {
      const combined = before === undefined ? result.value.messages : [...previous, ...result.value.messages];
      return [...new Map(combined.map((message) => [message.id, message])).values()].sort((a, b) => b.id - a.id);
    });
    setNextBefore(result.value.next_before);
  }, [channel, channelId, onMissing, onUnauthorized, closeContext]);

  useEffect(() => {
    const reconnect = previousLive.current !== 'open' && live === 'open';
    if (previousLive.current === 'open' && live !== 'open') {
      request.current++;
      closeContext();
      setBusy(false);
    }
    previousLive.current = live;
    if (reconnect && attempted.current !== '') void search(attempted.current);
  }, [live, search, closeContext]);

  useEffect(() => {
    if (context === null || context.error || context.messages.length === 0) return;
    const target = contextView.current?.querySelector<HTMLElement>(`[data-message-id="${String(context.id)}"]`);
    target?.scrollIntoView({ block: 'center' });
    target?.focus({ preventScroll: true });
  }, [context]);

  useEffect(() => {
    if (context !== null || returnTo.current === null) return;
    const id = returnTo.current;
    returnTo.current = null;
    resultView.current?.querySelector<HTMLElement>(`[data-message-id="${String(id)}"]`)?.focus();
  }, [context]);

  async function openContext(id: number) {
    const current = ++contextRequest.current;
    returnTo.current = id;
    setContext({ id, messages: [], error: false });
    const [before, after] = await Promise.all([api.loadMessages(channel, id), api.loadMessagesAfter(channel, id - 1)]);
    if (current !== contextRequest.current) return;
    if (!before.ok || !after.ok) {
      const unauthorized = (!before.ok && before.failure === 'unauthorized') || (!after.ok && after.failure === 'unauthorized');
      const missing = (!before.ok && before.code === 'channel_not_found') || (!after.ok && after.code === 'channel_not_found');
      if (unauthorized || missing) {
        setResults([]);
        setNextBefore(undefined);
        closeContext();
        if (unauthorized) onUnauthorized?.();
        else onMissing(channelId);
        return;
      }
      setContext({ id, messages: [], error: true });
      return;
    }
    const messages = [...new Map([...before.value, ...after.value].map((message) => [message.id, message])).values()].sort((a, b) => a.id - b.id);
    setContext({ id, messages, error: false });
  }

  const messageProps = { channel, people, nets, viewer, sent: false, cont: false, onUnauthorized };
  return (
    <section className="search-panel" aria-label={`Search #${channel}`}>
      <header className="conv-head">
        <h2>Search #{channel}</h2>
        <button type="button" onClick={onClose}>Close search</button>
      </header>
      <div ref={resultView} className="search-results-view" hidden={context !== null}>
        <form className="search-form" onSubmit={(event) => {
          event.preventDefault();
          const words = draft.trim();
          if (words === '') return;
          closeContext();
          void search(words);
        }}>
          <label htmlFor="channel-search">Messages in #{channel}</label>
          <div className="search-query">
            <input id="channel-search" type="search" autoFocus value={draft} onChange={(event) => {
              setDraft(event.target.value);
            }} />
            <button type="submit" disabled={draft.trim() === ''}>Search</button>
          </div>
        </form>
        {live === 'reconnecting' ? <p className="stale banner">Reconnecting; search results may be out of date.</p> : null}
        {busy ? <p role="status">Searching…</p> : null}
        {error ? (
          <div className="banner problem" role="alert">
            <p>Cannot search this channel.</p>
            <button type="button" onClick={() => {
              void search(attempted.current);
            }}>Try again</button>
          </div>
        ) : null}
        {!busy && !error && query !== '' && results.length === 0 ? <p role="status">No messages match “{query}”.</p> : null}
        {results.length === 0 ? null : (
          <div className="log search-results">
            <p className="hint">Newest matches first</p>
            <ol className="messages" aria-label="Search results">
              {results.map((message) => <Message key={message.id} message={message} {...messageProps} onOpen={(id) => {
                void openContext(id);
              }} />)}
            </ol>
          </div>
        )}
        {nextBefore === undefined ? null : (
          <button type="button" disabled={busy} onClick={() => {
            void search(query, nextBefore);
          }}>Load older results</button>
        )}
      </div>
      {context === null ? null : (
        <div ref={contextView} className="search-context">
          <button type="button" onClick={closeContext}>Back to results</button>
          <h3>Context around message #{context.id}</h3>
          {context.error ? <p role="alert">Cannot load message context.</p> : null}
          {!context.error && context.messages.length === 0 ? <p role="status">Loading context…</p> : null}
          <div className="log">
            <ol className="messages" aria-label="Message context">
              {context.messages.map((message) => <Message key={message.id} message={message} {...messageProps} />)}
            </ol>
          </div>
        </div>
      )}
    </section>
  );
}
