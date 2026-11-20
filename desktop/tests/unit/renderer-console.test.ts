import { EventEmitter } from 'node:events';
import type { WebContents } from 'electron';
import { describe, expect, it, vi } from 'vitest';
import { forwardVoiceConsole } from '../../main/voice/renderer-console.ts';

function renderer() {
  const log = vi.fn();
  const mainFrame = {};
  const contents = Object.assign(new EventEmitter(), { mainFrame });
  let time = 0;
  forwardVoiceConsole(contents as unknown as Pick<WebContents, 'on' | 'mainFrame'>, log, () => time);
  return {
    log,
    advance: (ms: number) => { time += ms; },
    say: (level: string, message: string, frame: object = mainFrame) => {
      contents.emit('console-message', {
        level, message, frame, sourceId: 'https://private-host/room/token', lineNumber: 23,
      });
    },
  };
}

describe('voice renderer console', () => {
  it('forwards the owned frame’s stall warning and discards other frames, info and unrelated logs', () => {
    const { say, log } = renderer();
    const stall = 'Screen capture stopped delivering frames (muted for 3 seconds).';
    say('info', stall);
    say('warning', stall, {});
    say('error', 'Unrelated renderer failure');
    say('warning', stall);
    expect(log).toHaveBeenCalledExactlyOnceWith(`conch-desktop: voice renderer warning: ${stall}`);
  });

  it('keeps only fixed diagnostics when renderer messages contain URLs, tokens, room names or SDP', () => {
    const { say, log } = renderer();
    const secrets = 'wss://private-host/?room=private-room auth=eyJabc.eyJdef.signature a=ice-pwd:private';
    say('error', `RTCPeerConnection failed: ${secrets}\nforged log entry`);
    say('error', `Bundled payload type collision: ${secrets}`);
    say('warning', `LiveKit SDK warning. ${secrets}`);
    say('error', 'LiveKit SDP description rejected due to a BUNDLE codec collision.');
    expect(log.mock.calls).toEqual([
      ['conch-desktop: voice renderer error: WebRTC reported a renderer warning or error.'],
      ['conch-desktop: voice renderer error: WebRTC reported a BUNDLE collision.'],
      ['conch-desktop: voice renderer error: LiveKit SDP description rejected due to a BUNDLE codec collision.'],
    ]);
  });

  it('rate-limits each diagnostic without hiding distinct events', () => {
    const { say, log, advance } = renderer();
    say('warning', 'LiveKit SDK warning.');
    say('warning', 'LiveKit SDK warning.');
    say('error', 'LiveKit SDK error.');
    say('warning', 'Screen share encoder is CPU-limited.');
    advance(9999);
    say('warning', 'LiveKit SDK warning.');
    expect(log).toHaveBeenCalledTimes(3);
    advance(1);
    say('warning', 'LiveKit SDK warning.');
    expect(log).toHaveBeenCalledTimes(4);
  });
});
