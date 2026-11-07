import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as api from './api';
import { SetTopic } from './SetTopic';
import { MAX_CHANNEL_TOPIC_LENGTH, type ChannelV0 } from './schema/channel';

vi.mock('./api', async (importOriginal) => {
  const real = await importOriginal<typeof import('./api')>();
  return { ...real, setChannelTopic: vi.fn() };
});

const setTopic = vi.mocked(api.setChannelTopic);
const channel: ChannelV0 = { id: 7, name: 'general', created_at: '2026-11-07T00:00:00Z', topic: 'Old topic' };

function form() {
  const onSet = vi.fn();
  const onUnauthorized = vi.fn();
  const onClose = vi.fn();
  render(<SetTopic channel={channel} onSet={onSet} onUnauthorized={onUnauthorized} onClose={onClose} />);
  return { onSet, onUnauthorized, onClose };
}

beforeEach(() => {
  setTopic.mockReset();
});
afterEach(cleanup);

describe('SetTopic', () => {
  it('prefills and bounds the topic, then hands the accepted channel back once', async () => {
    const updated = { ...channel, topic: 'What this room is for' };
    setTopic.mockResolvedValue({ ok: true, value: updated });
    const { onSet, onClose } = form();
    const field = screen.getByRole('textbox', { name: 'Topic' });
    expect(field).toHaveProperty('value', channel.topic);
    expect(field).toHaveProperty('maxLength', MAX_CHANNEL_TOPIC_LENGTH);
    fireEvent.change(field, { target: { value: updated.topic } });
    fireEvent.click(screen.getByRole('button', { name: 'Set topic' }));
    await waitFor(() => {
      expect(onSet).toHaveBeenCalledExactlyOnceWith(updated);
      expect(onClose).toHaveBeenCalledOnce();
    });
    expect(setTopic).toHaveBeenCalledExactlyOnceWith(channel.id, updated.topic);
  });

  it('Clear sends an empty topic and closes after the server accepts it', async () => {
    const updated = { ...channel, topic: undefined };
    setTopic.mockResolvedValue({ ok: true, value: updated });
    const { onSet, onClose } = form();
    fireEvent.click(screen.getByRole('button', { name: 'Clear' }));
    await waitFor(() => {
      expect(onSet).toHaveBeenCalledExactlyOnceWith(updated);
      expect(onClose).toHaveBeenCalledOnce();
    });
    expect(setTopic).toHaveBeenCalledExactlyOnceWith(channel.id, '');
  });

  it.each([
    [{ ok: false, failure: 'invalid', code: 'invalid_request' } as const, /That topic cannot be used/],
    [{ ok: false, failure: 'invalid', code: 'forbidden' } as const, 'You no longer manage this channel.'],
    [{ ok: false, failure: 'network' } as const, 'Cannot reach the server. Try again.'],
  ])('keeps the typed text and explains a refusal: %j', async (result, line) => {
    setTopic.mockResolvedValue(result);
    const { onSet, onClose } = form();
    const field = screen.getByRole('textbox', { name: 'Topic' });
    fireEvent.change(field, { target: { value: 'Still my typed topic' } });
    fireEvent.click(screen.getByRole('button', { name: 'Set topic' }));
    expect(await screen.findByText(line)).toHaveProperty('role', 'status');
    expect(field).toHaveProperty('value', 'Still my typed topic');
    expect(onSet).not.toHaveBeenCalled();
    expect(onClose).not.toHaveBeenCalled();
  });

  it('never sends twice while one request is in flight', async () => {
    let answer: (result: Awaited<ReturnType<typeof api.setChannelTopic>>) => void = () => undefined;
    setTopic.mockReturnValue(new Promise((resolve) => {
      answer = resolve;
    }));
    const { onSet } = form();
    const field = screen.getByRole('textbox', { name: 'Topic' });
    fireEvent.change(field, { target: { value: 'Only once' } });
    const send = screen.getByRole('button', { name: 'Set topic' });
    fireEvent.click(send);
    fireEvent.click(send);
    fireEvent.click(screen.getByRole('button', { name: 'Clear' }));
    expect(setTopic).toHaveBeenCalledExactlyOnceWith(channel.id, 'Only once');
    expect(send).toHaveProperty('disabled', true);
    const updated = { ...channel, topic: 'Only once' };
    await act(async () => {
      answer({ ok: true, value: updated });
      await Promise.resolve();
    });
    expect(onSet).toHaveBeenCalledExactlyOnceWith(updated);
  });
});
