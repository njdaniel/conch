import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { Conversation } from './Conversation';
import type { ChannelChat } from './state/messages';

const chat: ChannelChat = { byId: new Map(), pending: [], sentIds: new Set(), history: 'ready', older: 'idle', exhausted: true, trimmed: false, dropped: 0, nextLocalId: 1, edits: new Map(), final: new Set() };

function view(topic?: string) {
  return render(
    <Conversation
      channelId={7}
      channel="general"
      topic={topic}
      chat={chat}
      live="open"
      people={new Map()}
      nets={new Map()}
      viewer={2}
      draft=""
      target={{ net: null, whisper: null, refusal: null, note: null }}
      monitored={new Set()}
      onDraft={() => undefined}
      onTarget={() => undefined}
      onSend={() => undefined}
      onReplySend={() => undefined}
      onRetrySend={() => undefined}
      onDismissSend={() => undefined}
      onLoadOlder={() => undefined}
      onReload={() => undefined}
      onBottom={() => undefined}
    />,
  );
}

afterEach(cleanup);

describe('the channel topic in the conversation header', () => {
  it('shows the topic under the channel name', async () => {
    const { container } = view('Incidents and follow-ups');
    const topic = await screen.findByText('Incidents and follow-ups');
    const heading = await screen.findByRole('heading', { name: 'general' });
    expect(topic.parentElement).toBe(heading.parentElement);
    expect(container.querySelector('.conv-head .channel-topic')?.textContent).toBe('Incidents and follow-ups');
  });

  it('draws no topic line when the channel has none', async () => {
    const { container } = view();
    await screen.findByRole('heading', { name: 'general' });
    expect(container.querySelector('.conv-head .channel-topic')).toBeNull();
  });

  it('renders hostile-looking text literally, with no child element', async () => {
    const hostile = '<b>x</b> & y';
    view(hostile);
    const topic = await screen.findByText(hostile);
    expect(topic.textContent).toBe(hostile);
    expect(topic.children).toHaveLength(0);
    expect(topic.querySelector('b')).toBeNull();
  });
});
