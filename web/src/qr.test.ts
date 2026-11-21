// The QR encoder (issue #862), proved by a round trip: a decoder written here
// from the standard reads the matrix back (format information, unmasking, the
// zigzag, de-interleaving, a Reed-Solomon syndrome check that is zero, byte
// mode), and the matrix's structure is checked on its own. The decoder shares
// no code with qr.ts, and derives the block layout from the standard's totals
// rather than from qr.ts's table.
import { describe, expect, it } from 'vitest';
import { encodeQR, fitsQR, type QR } from './qr';

const totals = [26, 44, 70, 100, 134, 172, 196, 242, 292, 346];
const ecPerBlock = [10, 16, 26, 18, 24, 16, 18, 22, 22, 26];
const blockCount = [1, 1, 1, 2, 2, 4, 4, 4, 5, 5];
// Byte-mode capacities at level M, from the standard's table.
const capacity = [14, 26, 42, 62, 84, 106, 122, 152, 180, 213];
const alignmentCentres = [[], [6, 18], [6, 22], [6, 26], [6, 30], [6, 34], [6, 22, 38], [6, 24, 42], [6, 26, 46], [6, 28, 50]];

const exp: number[] = [];
const log: number[] = [];
{
  let x = 1;
  for (let i = 0; i < 255; i++) {
    exp[i] = x;
    log[x] = i;
    x <<= 1;
    if (x & 0x100) x ^= 0x11d;
  }
}
const mul = (a: number, b: number): number => (a === 0 || b === 0 ? 0 : (exp[((log[a] ?? 0) + (log[b] ?? 0)) % 255] ?? 0));

function bch(value: number, bits: number, poly: number, degree: number): number {
  let v = value << degree;
  for (let i = bits + degree - 1; i >= degree; i--) if ((v >> i) & 1) v ^= poly << (i - degree);
  return (value << degree) | v;
}

function dark(code: QR, x: number, y: number): number {
  return code.modules[y]?.[x] === true ? 1 : 0;
}

function isFunction(version: number, x: number, y: number): boolean {
  const n = version * 4 + 17;
  if ((x < 9 && y < 9) || (x >= n - 8 && y < 9) || (x < 9 && y >= n - 8)) return true;
  if (x === 6 || y === 6) return true;
  if (version >= 7 && ((x >= n - 11 && x < n - 8 && y < 6) || (y >= n - 11 && y < n - 8 && x < 6))) return true;
  const c = alignmentCentres[version - 1] ?? [];
  for (const cy of c)
    for (const cx of c) {
      const corner = (cx === 6 && cy === 6) || (cx === 6 && cy === c[c.length - 1]) || (cy === 6 && cx === c[c.length - 1]);
      if (!corner && Math.abs(x - cx) <= 2 && Math.abs(y - cy) <= 2) return true;
    }
  return false;
}

const masks: readonly ((x: number, y: number) => boolean)[] = [
  (x, y) => (x + y) % 2 === 0,
  (_x, y) => y % 2 === 0,
  (x) => x % 3 === 0,
  (x, y) => (x + y) % 3 === 0,
  (x, y) => (Math.floor(y / 2) + Math.floor(x / 3)) % 2 === 0,
  (x, y) => ((x * y) % 2) + ((x * y) % 3) === 0,
  (x, y) => (((x * y) % 2) + ((x * y) % 3)) % 2 === 0,
  (x, y) => (((x + y) % 2) + ((x * y) % 3)) % 2 === 0,
];

// Where the two copies of the 15 format bits are (bit i is the i-th least).
function formatCells(n: number): [[number, number][], [number, number][]] {
  const a: [number, number][] = [
    ...[0, 1, 2, 3, 4, 5].map((i): [number, number] => [8, i]),
    [8, 7],
    [8, 8],
    [7, 8],
    ...[9, 10, 11, 12, 13, 14].map((i): [number, number] => [14 - i, 8]),
  ];
  const b: [number, number][] = [
    ...[0, 1, 2, 3, 4, 5, 6, 7].map((i): [number, number] => [n - 1 - i, 8]),
    ...[8, 9, 10, 11, 12, 13, 14].map((i): [number, number] => [8, n - 15 + i]),
  ];
  return [a, b];
}

function formatCopies(code: QR): [number, number] {
  const [a, b] = formatCells(code.modules.length);
  const read = (cells: [number, number][]) => cells.reduce((v, [x, y], i) => v | (dark(code, x, y) << i), 0);
  return [read(a), read(b)];
}

function decode(code: QR): string {
  const n = code.modules.length;
  const version = (n - 17) / 4;
  expect(Number.isInteger(version)).toBe(true);

  // Format information: level M (00) and the mask, BCH-protected, and xor.
  const [first, second] = formatCopies(code);
  expect(second).toBe(first);
  const valid = Array.from({ length: 32 }, (_, d) => bch(d, 5, 0x537, 10) ^ 0x5412);
  const at = valid.indexOf(first);
  expect(at).toBeGreaterThanOrEqual(0);
  expect(at >> 3).toBe(0);
  const mask = at & 7;
  expect(mask).toBe(code.mask);

  // Version information, from both blocks, for version 7 and up.
  if (version >= 7) {
    const want = bch(version, 6, 0x1f25, 12);
    let a = 0;
    let b = 0;
    for (let i = 0; i < 18; i++) {
      a |= dark(code, n - 11 + (i % 3), Math.floor(i / 3)) << i;
      b |= dark(code, Math.floor(i / 3), n - 11 + (i % 3)) << i;
    }
    expect(a).toBe(want);
    expect(b).toBe(want);
  }

  // The zigzag, unmasked.
  const bits: number[] = [];
  let upward = true;
  for (let right = n - 1; right >= 1; right -= 2) {
    if (right === 6) right = 5;
    for (let k = 0; k < n; k++) {
      const y = upward ? n - 1 - k : k;
      for (const x of [right, right - 1]) {
        if (isFunction(version, x, y)) continue;
        bits.push(dark(code, x, y) ^ (masks[mask]?.(x, y) === true ? 1 : 0));
      }
    }
    upward = !upward;
  }
  const total = totals[version - 1] ?? 0;
  expect(bits.length - total * 8).toBeLessThan(8); // remainder bits only
  const words = Array.from({ length: total }, (_, i) => bits.slice(i * 8, i * 8 + 8).reduce((v, b) => (v << 1) | b, 0));

  // De-interleave: short blocks first, then the long ones (one more byte).
  const blocks = blockCount[version - 1] ?? 1;
  const ec = ecPerBlock[version - 1] ?? 0;
  const dataTotal = total - ec * blocks;
  const lengths = Array.from({ length: blocks }, (_, i) => Math.floor(dataTotal / blocks) + (i >= blocks - (dataTotal % blocks) ? 1 : 0));
  const data: number[][] = lengths.map(() => []);
  const checks: number[][] = lengths.map(() => []);
  let p = 0;
  for (let i = 0; i < Math.max(...lengths); i++) lengths.forEach((len, b) => i < len && data[b]?.push(words[p++] ?? 0));
  for (let i = 0; i < ec; i++) lengths.forEach((_, b) => checks[b]?.push(words[p++] ?? 0));
  expect(p).toBe(total);

  // Reed-Solomon: each block, data then check, vanishes at alpha^0 .. alpha^(ec-1).
  data.forEach((d, b) => {
    const poly = [...d, ...(checks[b] ?? [])];
    for (let r = 0; r < ec; r++) {
      let acc = 0;
      for (const c of poly) acc = mul(acc, exp[r] ?? 1) ^ c;
      expect(acc).toBe(0);
    }
  });

  // Byte mode.
  const stream = data.flat().flatMap((w) => Array.from({ length: 8 }, (_, i) => (w >> (7 - i)) & 1));
  const take = (from: number, count: number) => stream.slice(from, from + count).reduce((v, b) => (v << 1) | b, 0);
  expect(take(0, 4)).toBe(0b0100);
  const countBits = version < 10 ? 8 : 16;
  const length = take(4, countBits);
  const bytes = Array.from({ length }, (_, i) => take(4 + countBits + 8 * i, 8));
  // Padding after the terminator is 0xEC, 0x11 alternating.
  const used = Math.ceil((4 + countBits + 8 * length + 4) / 8);
  const pad = data.flat().slice(used);
  pad.forEach((w, i) => { expect(w).toBe(i % 2 === 0 ? 0xec : 0x11); });
  return new TextDecoder().decode(new Uint8Array(bytes));
}

// An independent penalty score (the standard's rules N1 to N4).
function score(code: QR, mask: number): number {
  const n = code.modules.length;
  const version = (n - 17) / 4;
  // The matrix with another mask in place of the chosen one: undo, redo.
  const grid = code.modules.map((row, y) => row.map((d, x) => (isFunction(version, x, y) ? d : d !== (masks[code.mask]?.(x, y) === true) !== (masks[mask]?.(x, y) === true))));
  // The format bits are part of the matrix a mask is judged on.
  const word = bch(mask, 5, 0x537, 10) ^ 0x5412;
  for (const cells of formatCells(n))
    cells.forEach(([x, y], i) => {
      const row = grid[y];
      if (row !== undefined) row[x] = ((word >> i) & 1) === 1;
    });
  const lines = [...grid.map((r) => r.map(Number)), ...grid.map((_, x) => grid.map((r) => Number(r[x])))];
  let s = 0;
  for (const line of lines) {
    const text = line.join('');
    for (const m of text.matchAll(/0{5,}|1{5,}/g)) s += m[0].length - 2;
    s += 40 * ((text.match(/(?=10111010000)/g) ?? []).length + (text.match(/(?=00001011101)/g) ?? []).length);
  }
  for (let y = 0; y + 1 < n; y++)
    for (let x = 0; x + 1 < n; x++) {
      const v = grid[y]?.[x];
      if (v === grid[y]?.[x + 1] && v === grid[y + 1]?.[x] && v === grid[y + 1]?.[x + 1]) s += 3;
    }
  const ones = grid.flat().filter(Boolean).length;
  const percent = (ones * 100) / (n * n);
  s += 10 * Math.max(0, Math.ceil(Math.abs(percent - 50) / 5) - 1);
  return s;
}

function structure(code: QR) {
  const n = code.modules.length;
  const finder = (ox: number, oy: number) => {
    for (let dy = 0; dy < 7; dy++)
      for (let dx = 0; dx < 7; dx++) {
        const edge = Math.max(Math.abs(dx - 3), Math.abs(dy - 3));
        expect(dark(code, ox + dx, oy + dy)).toBe(edge === 2 ? 0 : 1);
      }
  };
  finder(0, 0);
  finder(n - 7, 0);
  finder(0, n - 7);
  // Separators are light, the timing patterns alternate, the dark module is dark.
  for (let i = 0; i < 8; i++) {
    expect(dark(code, i, 7)).toBe(0);
    expect(dark(code, 7, i)).toBe(0);
    expect(dark(code, n - 8, i)).toBe(0);
    expect(dark(code, n - 1 - i, 7)).toBe(0);
    expect(dark(code, i, n - 8)).toBe(0);
    expect(dark(code, 7, n - 1 - i)).toBe(0);
  }
  for (let i = 8; i < n - 8; i++) {
    expect(dark(code, i, 6)).toBe(i % 2 === 0 ? 1 : 0);
    expect(dark(code, 6, i)).toBe(i % 2 === 0 ? 1 : 0);
  }
  expect(dark(code, 8, n - 8)).toBe(1);
  // Alignment patterns: dark centre, light ring, dark border.
  const c = alignmentCentres[(n - 17) / 4 - 1] ?? [];
  for (const cy of c)
    for (const cx of c) {
      if ((cx === 6 && cy === 6) || (cx === 6 && cy === c[c.length - 1]) || (cy === 6 && cx === c[c.length - 1])) continue;
      for (let dy = -2; dy <= 2; dy++) for (let dx = -2; dx <= 2; dx++) expect(dark(code, cx + dx, cy + dy)).toBe(Math.max(Math.abs(dx), Math.abs(dy)) === 1 ? 0 : 1);
    }
}

const sample = 'otpauth://totp/Conch:root?secret=JBSWY3DPEHPK3PXP&issuer=Conch';

describe('encodeQR', () => {
  it('round-trips a range of texts through an independent decoder', () => {
    const texts = [
      '',
      'A',
      'hello world',
      sample,
      'otpauth://totp/conch.example:root%40conch.example?secret=JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP&issuer=conch.example&algorithm=SHA1&digits=6&period=30',
      'café ☕ — ünïcode',
      '\u0000ÿ binary-ish \n\t',
    ];
    for (const text of texts) {
      const code = encodeQR(text);
      expect(code).not.toBeNull();
      if (code === null) continue;
      structure(code);
      expect(decode(code)).toBe(text);
    }
  });

  it('uses the smallest version, and round-trips each version at its capacity and one byte past it', () => {
    capacity.forEach((cap, i) => {
      const version = i + 1;
      for (const length of [cap, cap + 1]) {
        if (length > 213) continue;
        const text = Array.from({ length }, (_, k) => String.fromCharCode(33 + ((k * 7 + version) % 90))).join('');
        const code = encodeQR(text);
        expect(code?.version).toBe(length === cap ? version : version + 1);
        if (code === null) continue;
        structure(code);
        expect(decode(code)).toBe(text);
      }
    });
  });

  it('does not fit past version 10 at level M', () => {
    expect(encodeQR('x'.repeat(213))?.version).toBe(10);
    expect(encodeQR('x'.repeat(214))).toBeNull();
    expect(fitsQR('x'.repeat(213))).toBe(true);
    expect(fitsQR('x'.repeat(214))).toBe(false);
    // Bytes, not characters.
    expect(fitsQR('é'.repeat(107))).toBe(false);
    expect(fitsQR('é'.repeat(106))).toBe(true);
  });

  it('chooses the mask with the lowest penalty, and is deterministic', () => {
    for (const text of [sample, 'A', 'x'.repeat(150), 'otpauth://totp/' + 'ab'.repeat(30)]) {
      const code = encodeQR(text);
      if (code === null) throw new Error('should fit');
      const scores = masks.map((_, m) => score(code, m));
      expect(scores[code.mask]).toBe(Math.min(...scores));
      expect(scores.indexOf(Math.min(...scores))).toBe(code.mask);
      expect(encodeQR(text)?.modules).toEqual(code.modules);
    }
  });

  it('draws the sample URI at version 4', () => {
    const code = encodeQR(sample);
    expect(code?.version).toBe(4);
    expect(code?.modules).toHaveLength(33);
  });
});
