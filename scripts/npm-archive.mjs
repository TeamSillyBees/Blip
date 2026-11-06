import { gunzipSync, gzipSync } from 'node:zlib';

// Windows cannot represent Unix executable bits, and npm pack preserves that
// limitation. Set only the executable's tar mode so packages built on Windows
// also work on Unix without an install script or a second public CLI command.
export function executableArchive(archive, executable) {
  const tar = gunzipSync(archive);
  let found = false;
  for (let offset = 0; offset + 512 <= tar.length; ) {
    const header = tar.subarray(offset, offset + 512);
    if (header.every((byte) => byte === 0)) break;
    const name = header.toString('utf8', 0, 100).replace(/\0.*$/s, '');
    const prefix = header.toString('utf8', 345, 500).replace(/\0.*$/s, '');
    const path = prefix ? `${prefix}/${name}` : name;
    const size = Number.parseInt(header.toString('ascii', 124, 136).replace(/\0.*$/s, '').trim(), 8);
    if (!Number.isSafeInteger(size) || size < 0 || offset + 512 + size > tar.length) {
      throw new Error('Invalid npm tar archive size.');
    }
    if (path === executable) {
      if (header[156] !== 0 && header[156] !== 48) throw new Error(`${executable} must be a regular file.`);
      header.write('0000755\0', 100, 8, 'ascii');
      header.fill(32, 148, 156);
      const checksum = header.reduce((total, byte) => total + byte, 0);
      header.write(`${checksum.toString(8).padStart(6, '0')}\0 `, 148, 8, 'ascii');
      found = true;
    }
    offset += 512 + Math.ceil(size / 512) * 512;
  }
  if (!found) throw new Error(`Native executable ${executable} is missing from npm archive.`);
  return gzipSync(tar, { level: 9 });
}
