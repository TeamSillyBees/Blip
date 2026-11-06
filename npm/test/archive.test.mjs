import assert from 'node:assert/strict';
import { test } from 'node:test';
import { gunzipSync, gzipSync } from 'node:zlib';
import { executableArchive } from '../../scripts/npm-archive.mjs';

// A minimal POSIX ustar fixture containing a non-executable native binary.
function fixture() {
  const tar = Buffer.alloc(2048);
  tar.write('package/bin/blip', 0);
  tar.write('0000644\0', 100);
  tar.write('0000000\0', 108);
  tar.write('0000000\0', 116);
  tar.write('00000000004\0', 124);
  tar.write('00000000000\0', 136);
  tar.fill(' ', 148, 156);
  tar.write('0', 156);
  tar.write('ustar\0', 257);
  tar.write('00', 263);
  const checksum = tar.subarray(0, 512).reduce((sum, value) => sum + value, 0);
  tar.write(`${checksum.toString(8).padStart(6, '0')}\0 `, 148);
  tar.write('data', 512);
  return tar;
}

test('Windows-packed native archives receive Unix execute permission with a valid checksum', () => {
  const before = fixture();
  const archive = executableArchive(gzipSync(before), 'package/bin/blip');
  const after = gunzipSync(archive);
  assert.equal(after.toString('ascii', 100, 108), '0000755\0');
  const checksum = Number.parseInt(after.toString('ascii', 148, 154), 8);
  const header = Buffer.from(after.subarray(0, 512));
  header.fill(' ', 148, 156);
  assert.equal(checksum, [...header].reduce((sum, value) => sum + value, 0));
  assert.deepEqual(after.subarray(512), before.subarray(512), 'File data and archive padding must not change.');
  assert.deepEqual(executableArchive(gzipSync(before), 'package/bin/blip'), archive, 'Packing is deterministic.');
});

test('archive normalization rejects a missing executable and a truncated entry', () => {
  assert.throws(() => executableArchive(gzipSync(fixture()), 'package/bin/absent'), /missing/);
  const malformed = fixture();
  malformed.write('77777777777\0', 124);
  assert.throws(() => executableArchive(gzipSync(malformed), 'package/bin/blip'), /Invalid npm tar archive size/);
});
