'use strict';

const { spawnSync } = require('node:child_process');
const { createHash } = require('node:crypto');
const { readFileSync } = require('node:fs');
const { dirname, join } = require('node:path');
const { getTarget } = require('./platforms.cjs');
const wrapper = require('../package.json');

function synchronize(binary, target = getTarget()) {
  const native = JSON.parse(readFileSync(join(dirname(binary), '..', 'package.json'), 'utf8'));
  const expected = wrapper.blipNativeSha256?.[target.id];
  if (native.name !== target.packageName || native.version !== wrapper.version ||
      !/^[0-9a-f]{64}$/.test(expected || '') ||
      createHash('sha256').update(readFileSync(binary)).digest('hex') !== expected) {
    throw new Error('Bundled Blip executable failed package version or SHA-256 verification. Reinstall the matching npm packages.');
  }
  const result = spawnSync(binary, ['sync-executable', expected], {
    encoding: 'utf8', windowsHide: true, stdio: ['ignore', 'pipe', 'pipe'],
  });
  if (result.error) throw result.error;
  if (result.status !== 0) throw new Error(result.stderr.trim() || 'Could not synchronize the fixed Blip executable.');
  const installed = result.stdout.trimEnd();
  if (!installed) throw new Error('Blip did not return its installed executable path.');
  return installed;
}

module.exports = { synchronize };
