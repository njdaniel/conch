// A QR Code encoder (ISO/IEC 18004), for one job: the authenticator's
// otpauth:// URI on the operator setup page (issue #862). Byte mode, error
// correction level M, versions 1 to 10 (up to 213 bytes at version 10), the
// mask chosen by the standard's four penalty rules.
//
// Input: a string, taken as UTF-8 bytes. Output: a square matrix of modules
// (true is dark) with the version and mask chosen, or null when the text does
// not fit version 10 at level M. It owns no state and touches nothing but its
// argument: the secret goes in, and only the caller decides where it goes.

export type QR = {
  readonly version: number;
  readonly mask: number;
  readonly modules: readonly (readonly boolean[])[];
};

// Level M, per version 1..10: error-correction codewords per block, and the
// blocks' data-codeword counts (ISO/IEC 18004 table 9).
const blocksM: readonly { readonly ec: number; readonly data: readonly number[] }[] = [
  { ec: 10, data: [16] },
  { ec: 16, data: [28] },
  { ec: 26, data: [44] },
  { ec: 18, data: [32, 32] },
  { ec: 24, data: [43, 43] },
  { ec: 16, data: [27, 27, 27, 27] },
  { ec: 18, data: [31, 31, 31, 31] },
  { ec: 22, data: [38, 38, 39, 39] },
  { ec: 22, data: [36, 36, 36, 37, 37] },
  { ec: 26, data: [43, 43, 43, 43, 44] },
];

const alignment: readonly (readonly number[])[] = [
  [],
  [6, 18],
  [6, 22],
  [6, 26],
  [6, 30],
  [6, 34],
  [6, 22, 38],
  [6, 24, 42],
  [6, 26, 46],
  [6, 28, 50],
];

export const maxVersion = blocksM.length;

function dataCodewords(version: number): number {
  const b = blocksM[version - 1];
  return b === undefined ? 0 : b.data.reduce((a, n) => a + n, 0);
}

function utf8(text: string): number[] {
  return Array.from(new TextEncoder().encode(text));
}

// The smallest version holding these bytes in byte mode at level M.
function versionFor(length: number): number | null {
  for (let v = 1; v <= maxVersion; v++) {
    const countBits = v < 10 ? 8 : 16;
    if (4 + countBits + 8 * length <= 8 * dataCodewords(v)) return v;
  }
  return null;
}

// fitsQR says whether the text can be drawn.
export function fitsQR(text: string): boolean {
  return versionFor(utf8(text).length) !== null;
}

// GF(256) with the polynomial 0x11D.
function gfMul(x: number, y: number): number {
  let z = 0;
  for (let i = 7; i >= 0; i--) {
    z = (z << 1) ^ ((z >>> 7) * 0x11d);
    z ^= ((y >>> i) & 1) * x;
  }
  return z;
}

function rsGenerator(degree: number): number[] {
  const result: number[] = new Array<number>(degree).fill(0);
  result[degree - 1] = 1;
  let root = 1;
  for (let i = 0; i < degree; i++) {
    for (let j = 0; j < degree; j++) {
      result[j] = gfMul(result[j] ?? 0, root);
      if (j + 1 < degree) result[j] = (result[j] ?? 0) ^ (result[j + 1] ?? 0);
    }
    root = gfMul(root, 0x02);
  }
  return result;
}

function rsRemainder(data: readonly number[], generator: readonly number[]): number[] {
  const result: number[] = new Array<number>(generator.length).fill(0);
  for (const b of data) {
    const factor = b ^ (result.shift() ?? 0);
    result.push(0);
    generator.forEach((coef, i) => {
      result[i] = (result[i] ?? 0) ^ gfMul(coef, factor);
    });
  }
  return result;
}

function bitsOf(value: number, count: number): number[] {
  const out: number[] = [];
  for (let i = count - 1; i >= 0; i--) out.push((value >>> i) & 1);
  return out;
}

// The data codewords: mode, count, bytes, terminator, padding.
function dataFor(bytes: readonly number[], version: number): number[] {
  const bits: number[] = [0, 1, 0, 0, ...bitsOf(bytes.length, version < 10 ? 8 : 16)];
  for (const b of bytes) bits.push(...bitsOf(b, 8));
  const capacity = 8 * dataCodewords(version);
  bits.push(...new Array<number>(Math.min(4, capacity - bits.length)).fill(0));
  while (bits.length % 8 !== 0) bits.push(0);
  const out: number[] = [];
  for (let i = 0; i < bits.length; i += 8) out.push(parseInt(bits.slice(i, i + 8).join(''), 2));
  for (let pad = 0xec; out.length < dataCodewords(version); pad ^= 0xec ^ 0x11) out.push(pad);
  return out;
}

// Split into blocks, add each one's error correction, interleave.
function interleaved(data: readonly number[], version: number): number[] {
  const spec = blocksM[version - 1];
  if (spec === undefined) return [];
  const generator = rsGenerator(spec.ec);
  const blocks: number[][] = [];
  let at = 0;
  for (const n of spec.data) {
    blocks.push(data.slice(at, at + n));
    at += n;
  }
  const checks = blocks.map((b) => rsRemainder(b, generator));
  const out: number[] = [];
  const longest = Math.max(...spec.data);
  for (let i = 0; i < longest; i++) for (const b of blocks) if (i < b.length) out.push(b[i] ?? 0);
  for (let i = 0; i < spec.ec; i++) for (const c of checks) out.push(c[i] ?? 0);
  return out;
}

class Grid {
  readonly size: number;
  readonly modules: boolean[][];
  readonly fixed: boolean[][];

  constructor(readonly version: number) {
    this.size = version * 4 + 17;
    this.modules = Array.from({ length: this.size }, () => new Array<boolean>(this.size).fill(false));
    this.fixed = Array.from({ length: this.size }, () => new Array<boolean>(this.size).fill(false));
    this.drawFunctionPatterns();
  }

  private set(x: number, y: number, dark: boolean): void {
    const row = this.modules[y];
    const fix = this.fixed[y];
    if (row === undefined || fix === undefined || x < 0 || x >= this.size) return;
    row[x] = dark;
    fix[x] = true;
  }

  private finder(cx: number, cy: number): void {
    for (let dy = -4; dy <= 4; dy++)
      for (let dx = -4; dx <= 4; dx++) {
        const dist = Math.max(Math.abs(dx), Math.abs(dy));
        this.set(cx + dx, cy + dy, dist !== 2 && dist !== 4);
      }
  }

  private drawFunctionPatterns(): void {
    for (let i = 0; i < this.size; i++) {
      this.set(6, i, i % 2 === 0);
      this.set(i, 6, i % 2 === 0);
    }
    this.finder(3, 3);
    this.finder(this.size - 4, 3);
    this.finder(3, this.size - 4);
    const at = alignment[this.version - 1] ?? [];
    const last = at.length - 1;
    at.forEach((cy, i) => { at.forEach((cx, j) => {
        if ((i === 0 && j === 0) || (i === 0 && j === last) || (i === last && j === 0)) return;
        for (let dy = -2; dy <= 2; dy++)
          for (let dx = -2; dx <= 2; dx++) this.set(cx + dx, cy + dy, Math.max(Math.abs(dx), Math.abs(dy)) !== 1);
      }); },
    );
    this.drawFormat(0);
    this.drawVersion();
  }

  // Format information: level M is 00, then the mask; BCH(15,5), then xor.
  drawFormat(mask: number): void {
    let rem = mask;
    for (let i = 0; i < 10; i++) rem = (rem << 1) ^ ((rem >>> 9) * 0x537);
    const bits = ((mask << 10) | rem) ^ 0x5412;
    const bit = (i: number): boolean => ((bits >>> i) & 1) !== 0;
    for (let i = 0; i <= 5; i++) this.set(8, i, bit(i));
    this.set(8, 7, bit(6));
    this.set(8, 8, bit(7));
    this.set(7, 8, bit(8));
    for (let i = 9; i < 15; i++) this.set(14 - i, 8, bit(i));
    for (let i = 0; i < 8; i++) this.set(this.size - 1 - i, 8, bit(i));
    for (let i = 8; i < 15; i++) this.set(8, this.size - 15 + i, bit(i));
    this.set(8, this.size - 8, true);
  }

  private drawVersion(): void {
    if (this.version < 7) return;
    let rem = this.version;
    for (let i = 0; i < 12; i++) rem = (rem << 1) ^ ((rem >>> 11) * 0x1f25);
    const bits = (this.version << 12) | rem;
    for (let i = 0; i < 18; i++) {
      const dark = ((bits >>> i) & 1) !== 0;
      const a = this.size - 11 + (i % 3);
      const b = Math.floor(i / 3);
      this.set(a, b, dark);
      this.set(b, a, dark);
    }
  }

  // The zigzag: two columns at a time from the right, up then down.
  place(codewords: readonly number[]): void {
    let i = 0;
    for (let right = this.size - 1; right >= 1; right -= 2) {
      if (right === 6) right = 5;
      for (let vert = 0; vert < this.size; vert++)
        for (let j = 0; j < 2; j++) {
          const x = right - j;
          const upward = ((right + 1) & 2) === 0;
          const y = upward ? this.size - 1 - vert : vert;
          const row = this.modules[y];
          if (row === undefined || this.fixed[y]?.[x] === true || i >= codewords.length * 8) continue;
          row[x] = (((codewords[i >>> 3] ?? 0) >>> (7 - (i & 7))) & 1) !== 0;
          i++;
        }
    }
  }

  mask(m: number): void {
    for (let y = 0; y < this.size; y++)
      for (let x = 0; x < this.size; x++) {
        const row = this.modules[y];
        if (row === undefined || this.fixed[y]?.[x] === true || !maskCondition(m, x, y)) continue;
        row[x] = !row[x];
      }
  }
}

function maskCondition(m: number, x: number, y: number): boolean {
  switch (m) {
    case 0:
      return (x + y) % 2 === 0;
    case 1:
      return y % 2 === 0;
    case 2:
      return x % 3 === 0;
    case 3:
      return (x + y) % 3 === 0;
    case 4:
      return (Math.floor(x / 3) + Math.floor(y / 2)) % 2 === 0;
    case 5:
      return ((x * y) % 2) + ((x * y) % 3) === 0;
    case 6:
      return (((x * y) % 2) + ((x * y) % 3)) % 2 === 0;
    default:
      return (((x + y) % 2) + ((x * y) % 3)) % 2 === 0;
  }
}

// The standard's penalty score: runs of five or more (3 + extra each), 2x2
// blocks (3 each), finder-like 1:1:3:1:1 runs with a light margin (40 each),
// and the dark share's distance from half (10 per 5%).
function penalty(modules: readonly (readonly boolean[])[]): number {
  const n = modules.length;
  const at = (x: number, y: number): boolean => modules[y]?.[x] === true;
  let score = 0;
  for (const horizontal of [true, false]) {
    for (let a = 0; a < n; a++) {
      const line = Array.from({ length: n }, (_, b) => (horizontal ? at(b, a) : at(a, b)));
      let run = 1;
      for (let b = 1; b <= n; b++) {
        if (b < n && line[b] === line[b - 1]) run++;
        else {
          if (run >= 5) score += run - 2;
          run = 1;
        }
      }
      const s = line.map((d) => (d ? '1' : '0')).join('');
      for (const pat of ['10111010000', '00001011101']) {
        for (let i = s.indexOf(pat); i !== -1; i = s.indexOf(pat, i + 1)) score += 40;
      }
    }
  }
  let dark = 0;
  for (let y = 0; y < n; y++)
    for (let x = 0; x < n; x++) {
      if (at(x, y)) dark++;
      if (x + 1 < n && y + 1 < n && at(x, y) === at(x + 1, y) && at(x, y) === at(x, y + 1) && at(x, y) === at(x + 1, y + 1)) score += 3;
    }
  const total = n * n;
  score += Math.max(0, Math.ceil(Math.abs(dark * 20 - total * 10) / total) - 1) * 10;
  return score;
}

// encodeQR draws text, or returns null when it does not fit.
export function encodeQR(text: string): QR | null {
  const bytes = utf8(text);
  const version = versionFor(bytes.length);
  if (version === null) return null;
  const grid = new Grid(version);
  grid.place(interleaved(dataFor(bytes, version), version));
  let best = 0;
  let bestScore = Infinity;
  for (let m = 0; m < 8; m++) {
    grid.mask(m);
    grid.drawFormat(m);
    const score = penalty(grid.modules);
    if (score < bestScore) {
      best = m;
      bestScore = score;
    }
    grid.mask(m);
  }
  grid.mask(best);
  grid.drawFormat(best);
  return { version, mask: best, modules: grid.modules };
}
