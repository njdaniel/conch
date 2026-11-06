import { describe, expect, it } from 'vitest';
import { formatSize, uploadRefusalLine } from './fileSize';

describe('formatSize', () => {
  it.each([[0, '0 B'], [812, '812 B'], [3400, '3.4 KB'], [10485760, '10 MB'], [1000, '1 KB']] as const)('0 is ', (n, text) => {
    expect(formatSize(n)).toBe(text);
  });
});

describe('uploadRefusalLine', () => {
  it('names the file and the limit for a 413', () => {
    expect(uploadRefusalLine('big.zip', 'file_too_large', 'a file may be at most 10485760 bytes')).toBe('big.zip is too large: this server takes files up to 10 MB.');
  });
  it('keeps the server line for other refusals', () => {
    expect(uploadRefusalLine('a.txt', 'uploads_disabled', 'file uploads are disabled on this server')).toBe('file uploads are disabled on this server');
  });
});
