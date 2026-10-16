// A loud tone for Chromium's fake microphone (--use-file-for-fake-audio-capture):
// a 440 Hz sine, 48 kHz mono 16-bit, 20 s, which the browser loops. The default
// fake device beeps now and then; a steady tone makes "the bytes rise while A
// holds the key and stay flat otherwise" a clean measurement, and is far above
// the open microphone's level.
import { existsSync, mkdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';

export function toneFile(): string {
  const dir = join(tmpdir(), 'conch-web-e2e-tone');
  const path = join(dir, 'tone-440.wav');
  if (existsSync(path)) return path;
  mkdirSync(dir, { recursive: true });
  const rate = 48000;
  const samples = rate * 20;
  const bytes = new Uint8Array(44 + samples * 2);
  const view = new DataView(bytes.buffer);
  const text = (at: number, s: string) => {
    for (let i = 0; i < s.length; i++) view.setUint8(at + i, s.charCodeAt(i));
  };
  text(0, 'RIFF');
  view.setUint32(4, 36 + samples * 2, true);
  text(8, 'WAVE');
  text(12, 'fmt ');
  view.setUint32(16, 16, true);
  view.setUint16(20, 1, true); // PCM
  view.setUint16(22, 1, true); // mono
  view.setUint32(24, rate, true);
  view.setUint32(28, rate * 2, true);
  view.setUint16(32, 2, true);
  view.setUint16(34, 16, true);
  text(36, 'data');
  view.setUint32(40, samples * 2, true);
  for (let i = 0; i < samples; i++) {
    view.setInt16(44 + i * 2, Math.round(Math.sin((2 * Math.PI * 440 * i) / rate) * 0.6 * 32767), true);
  }
  writeFileSync(path, bytes);
  return path;
}
