export const maxImageAxis = 8192;
export const maxImagePixels = 25_000_000;

export type ImageFormat = 'png' | 'jpeg';

export interface ImageInfo {
  readonly format: ImageFormat;
  readonly width: number;
  readonly height: number;
}

export type ImageValidationResult =
  | { readonly ok: true; readonly value: ImageInfo }
  | { readonly ok: false; readonly failure: 'unsupported' | 'invalid' | 'too-large' | 'unavailable' };

type Parsed = { readonly ok: true; readonly value: ImageInfo } | { readonly ok: false; readonly failure: 'unsupported' | 'invalid' | 'too-large' };

const pngSignature = [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a];
const sofMarkers = new Set([0xc0, 0xc1, 0xc2, 0xc3, 0xc5, 0xc6, 0xc7, 0xc9, 0xca, 0xcb, 0xcd, 0xce, 0xcf]);
const invalid = { ok: false, failure: 'invalid' } as const;
const unsupported = { ok: false, failure: 'unsupported' } as const;
const tooLarge = { ok: false, failure: 'too-large' } as const;

function u16(bytes: Uint8Array, at: number): number {
  return ((bytes[at] ?? 0) << 8) | (bytes[at + 1] ?? 0);
}

function u32(bytes: Uint8Array, at: number): number {
  return (bytes[at] ?? 0) * 0x1000000 + ((bytes[at + 1] ?? 0) << 16) + ((bytes[at + 2] ?? 0) << 8) + (bytes[at + 3] ?? 0);
}

function dimensions(format: ImageFormat, width: number, height: number): Parsed {
  if (width === 0 || height === 0) return invalid;
  if (width > maxImageAxis || height > maxImageAxis || width * height > maxImagePixels) return tooLarge;
  return { ok: true, value: { format, width, height } };
}

function parsePNG(bytes: Uint8Array): Parsed {
  if (bytes.length < 33 || u32(bytes, 8) !== 13 || u32(bytes, 12) !== 0x49484452) return invalid;
  return dimensions('png', u32(bytes, 16), u32(bytes, 20));
}

function parseJPEG(bytes: Uint8Array): Parsed {
  let at = 2;
  while (at < bytes.length) {
    if (bytes[at] !== 0xff) return invalid;
    while (bytes[at] === 0xff) at++;
    if (at >= bytes.length) return invalid;
    const marker = bytes[at++] ?? 0;
    if (marker === 0x00 || marker === 0xd8 || marker === 0xd9) return invalid;
    if (marker === 0x01 || (marker >= 0xd0 && marker <= 0xd7)) continue;
    if (at + 2 > bytes.length) return invalid;
    const length = u16(bytes, at);
    if (length < 2 || at + length > bytes.length) return invalid;
    if (sofMarkers.has(marker)) {
      const components = bytes[at + 7] ?? 0;
      if (length < 8 || components === 0 || length !== 8 + components * 3) return invalid;
      return dimensions('jpeg', u16(bytes, at + 5), u16(bytes, at + 3));
    }
    at += length;
  }
  return invalid;
}

function parse(bytes: Uint8Array): Parsed {
  const png = pngSignature.every((byte, index) => bytes[index] === byte);
  if (png) return parsePNG(bytes);
  if (bytes.length >= 2 && bytes[0] === 0xff && bytes[1] === 0xd8) return parseJPEG(bytes);
  const partialPNG = bytes.length < pngSignature.length && pngSignature.slice(0, bytes.length).every((byte, index) => bytes[index] === byte);
  return partialPNG || (bytes.length === 1 && bytes[0] === 0xff) ? invalid : unsupported;
}

// The encoded cap is a client-side resource limit, independent of the decoded
// dimension limits. Header parsing needs at most this bounded prefix; a JPEG
// whose SOF marker is later is rejected rather than read without a bound.
export const maxImageEncodedBytes = 20 * 1024 * 1024;
export const maxImageHeaderBytes = 64 * 1024;

// validateImage admits only PNG and JPEG bytes, whatever name or media type the
// Blob carries. The decoder is a second check, not a substitute for bounding
// the encoded input and header first. The returned metadata owns no bitmap or
// object URL.
export async function validateImage(blob: Blob): Promise<ImageValidationResult> {
  if (blob.size > maxImageEncodedBytes) return tooLarge;
  let bytes: Uint8Array;
  try {
    bytes = new Uint8Array(await blob.slice(0, maxImageHeaderBytes).arrayBuffer());
  } catch {
    return invalid;
  }
  const parsed = parse(bytes);
  if (!parsed.ok) return parsed;
  if (typeof createImageBitmap !== 'function') return { ok: false, failure: 'unavailable' };
  let bitmap: ImageBitmap;
  try {
    bitmap = await createImageBitmap(blob, { imageOrientation: 'none' });
  } catch {
    return invalid;
  }
  try {
    const decoded = dimensions(parsed.value.format, bitmap.width, bitmap.height);
    if (!decoded.ok) return decoded;
    if (bitmap.width !== parsed.value.width || bitmap.height !== parsed.value.height) return invalid;
    return parsed;
  } finally {
    bitmap.close();
  }
}
