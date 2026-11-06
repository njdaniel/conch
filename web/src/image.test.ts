import { afterEach, describe, expect, it, vi } from 'vitest';
import { maxImageAxis, maxImageEncodedBytes, maxImageHeaderBytes, maxImagePixels, validateImage } from './image';

afterEach(() => {
  vi.unstubAllGlobals();
});

function png(width: number, height: number): Uint8Array {
  const bytes = new Uint8Array(33);
  bytes.set([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 13, 0x49, 0x48, 0x44, 0x52]);
  new DataView(bytes.buffer).setUint32(16, width);
  new DataView(bytes.buffer).setUint32(20, height);
  return bytes;
}

function jpeg(width: number, height: number): Uint8Array {
  const bytes = new Uint8Array([
    0xff, 0xd8,
    0xff, 0xe0, 0, 4, 0, 0,
    0xff, 0xc0, 0, 11, 8, 0, 0, 0, 0, 1, 1, 0x11, 0,
    0xff, 0xd9,
  ]);
  new DataView(bytes.buffer).setUint16(13, height);
  new DataView(bytes.buffer).setUint16(15, width);
  return bytes;
}

function decodeAs(width: number, height: number) {
  const close = vi.fn();
  const decode = vi.fn().mockResolvedValue({ width, height, close });
  vi.stubGlobal('createImageBitmap', decode);
  return { decode, close };
}

const blob = (bytes: Uint8Array, type = 'application/octet-stream') => new Blob([bytes as BlobPart], { type });

describe('validateImage', () => {
  it.each([
    { format: 'png', bytes: () => png(640, 480) },
    { format: 'jpeg', bytes: () => jpeg(640, 480) },
  ])('validates and decodes $format bytes without trusting the Blob type', async ({ format, bytes }) => {
    const { decode, close } = decodeAs(640, 480);
    const input = blob(bytes(), 'text/html');
    expect(await validateImage(input)).toEqual({ ok: true, value: { format, width: 640, height: 480 } });
    expect(decode).toHaveBeenCalledWith(input, { imageOrientation: 'none' });
    expect(close).toHaveBeenCalledOnce();
  });

  it.each([
    { name: 'SVG', text: '<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>' },
    { name: 'HTML', text: '<html><img src=x onerror=alert(1)></html>' },
    { name: 'GIF', text: 'GIF89a' },
    { name: 'WebP', text: 'RIFF....WEBPVP8 ' },
  ])('refuses $name before decoding', async ({ text }) => {
    const { decode } = decodeAs(1, 1);
    expect(await validateImage(blob(new TextEncoder().encode(text), 'image/png'))).toEqual({ ok: false, failure: 'unsupported' });
    expect(decode).not.toHaveBeenCalled();
  });

  it('refuses every truncated PNG header before decoding', async () => {
    const { decode } = decodeAs(640, 480);
    const input = png(640, 480);
    for (let length = 1; length < input.length; length++) {
      expect((await validateImage(blob(input.slice(0, length)))).ok).toBe(false);
    }
    expect(decode).not.toHaveBeenCalled();
  });

  it('refuses every truncated JPEG header before decoding', async () => {
    const { decode } = decodeAs(640, 480);
    const input = jpeg(640, 480);
    for (let length = 1; length < 21; length++) {
      expect((await validateImage(blob(input.slice(0, length)))).ok).toBe(false);
    }
    expect(decode).not.toHaveBeenCalled();
  });

  it.each([
    { name: 'PNG with no complete IHDR', bytes: () => png(1, 1).fill(0, 8, 16) },
    { name: 'JPEG with a bad segment length', bytes: () => {
      const bytes = jpeg(1, 1);
      bytes[11] = 1;
      return bytes;
    } },
    { name: 'zero-width PNG', bytes: () => png(0, 1) },
    { name: 'zero-height JPEG', bytes: () => jpeg(1, 0) },
  ])('refuses $name', async ({ bytes }) => {
    const { decode } = decodeAs(1, 1);
    expect(await validateImage(blob(bytes()))).toEqual({ ok: false, failure: 'invalid' });
    expect(decode).not.toHaveBeenCalled();
  });

  it.each([
    { name: 'an axis over the cap', width: maxImageAxis + 1, height: 1 },
    { name: 'pixels over the cap', width: 5001, height: 5000 },
    { name: 'both axes large', width: maxImageAxis, height: maxImageAxis },
  ])('refuses $name before decoding', async ({ width, height }) => {
    const { decode } = decodeAs(width, height);
    expect(await validateImage(blob(png(width, height)))).toEqual({ ok: false, failure: 'too-large' });
    expect(decode).not.toHaveBeenCalled();
  });

  it('admits the exact pixel cap', async () => {
    const side = Math.sqrt(maxImagePixels);
    decodeAs(side, side);
    expect((await validateImage(blob(png(side, side)))).ok).toBe(true);
  });

  it('rejects excessive encoded input before reading or decoding it', async () => {
    const { decode } = decodeAs(10, 10);
    const input = blob(png(10, 10));
    Object.defineProperty(input, 'size', { value: maxImageEncodedBytes + 1 });
    const read = vi.spyOn(input, 'arrayBuffer');
    const slice = vi.spyOn(input, 'slice');
    expect(await validateImage(input)).toEqual({ ok: false, failure: 'too-large' });
    expect(read).not.toHaveBeenCalled();
    expect(slice).not.toHaveBeenCalled();
    expect(decode).not.toHaveBeenCalled();
  });

  it('reads only a bounded header slice of a huge Blob before decoding', async () => {
    const { decode } = decodeAs(10, 10);
    const input = blob(png(10, 10));
    Object.defineProperty(input, 'size', { value: maxImageEncodedBytes });
    const read = vi.spyOn(input, 'arrayBuffer');
    const slice = vi.spyOn(input, 'slice');
    expect(await validateImage(input)).toEqual({ ok: true, value: { format: 'png', width: 10, height: 10 } });
    expect(read).not.toHaveBeenCalled();
    expect(slice).toHaveBeenCalledOnce();
    expect(slice).toHaveBeenCalledWith(0, maxImageHeaderBytes);
    expect(decode).toHaveBeenCalledWith(input, { imageOrientation: 'none' });
  });

  it('requires the actual decoder to accept the bytes', async () => {
    const decode = vi.fn().mockRejectedValue(new DOMException('broken', 'InvalidStateError'));
    vi.stubGlobal('createImageBitmap', decode);
    expect(await validateImage(blob(png(10, 10)))).toEqual({ ok: false, failure: 'invalid' });
  });

  it('rechecks decoded dimensions and closes a mismatched bitmap', async () => {
    const { close } = decodeAs(11, 10);
    expect(await validateImage(blob(png(10, 10)))).toEqual({ ok: false, failure: 'invalid' });
    expect(close).toHaveBeenCalledOnce();
  });

  it('bounds decoded dimensions independently and closes the bitmap', async () => {
    const { close } = decodeAs(maxImageAxis + 1, 1);
    expect(await validateImage(blob(png(10, 10)))).toEqual({ ok: false, failure: 'too-large' });
    expect(close).toHaveBeenCalledOnce();
  });

  it('reports a browser without the required decoder', async () => {
    vi.stubGlobal('createImageBitmap', undefined);
    expect(await validateImage(blob(png(10, 10)))).toEqual({ ok: false, failure: 'unavailable' });
  });
});
