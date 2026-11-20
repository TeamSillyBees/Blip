import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import { join } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import platforms from '../npm/blip/lib/platforms.cjs';
import { runNpm } from './npm-helpers.mjs';

const registry = 'https://registry.npmjs.org';
const output = 'dist/npm';
const mode = process.argv[2];
assert.ok(process.argv.length === 3 && ['--dry-run', '--publish'].includes(mode), 'Use --dry-run or --publish.');
const live = mode === '--publish';
const { version, platforms: nativePackages, wrapper } = JSON.parse(await readFile(join(output, 'manifest.json'), 'utf8'));
const packages = [...nativePackages, wrapper];
assert.deepEqual(packages.map((pkg) => pkg.name), [...platforms.targets.map((target) => target.packageName), '@teamsillybees/blip']);
if (live) {
  assert.equal(process.env.GITHUB_EVENT_NAME, 'push');
  assert.equal(process.env.GITHUB_REF, `refs/tags/v${version}`, 'The tested packages must match the pushed tag.');
}

async function publishedIntegrity(name) {
  const response = await fetch(`${registry}/${name.replace('/', '%2f')}/${version}`, { cache: 'no-store' });
  if (response.status === 404) return null;
  if (!response.ok) throw new Error(`Could not read ${name}@${version}: HTTP ${response.status}`);
  const metadata = await response.json();
  assert.equal(metadata.version, version);
  assert.ok(metadata.dist?.integrity, `Missing registry integrity for ${name}@${version}`);
  return metadata.dist.integrity;
}

// npm publish --dry-run can succeed while logged out. Check that npm accepts
// the workflow identity for each package before reporting a successful rehearsal.
async function verifyTrustedPublishers() {
  const url = new URL(process.env.ACTIONS_ID_TOKEN_REQUEST_URL);
  url.searchParams.set('audience', 'npm:registry.npmjs.org');
  const identity = await fetch(url, { headers: { Authorization: `Bearer ${process.env.ACTIONS_ID_TOKEN_REQUEST_TOKEN}` } });
  if (!identity.ok) throw new Error(`GitHub OIDC returned HTTP ${identity.status}`);
  const { value } = await identity.json();
  assert.ok(value, 'GitHub did not supply a workflow identity.');
  for (const { name } of packages) {
    const exchange = await fetch(`${registry}/-/npm/v1/oidc/token/exchange/package/${name.replace('/', '%2f')}`, {
      method: 'POST',
      headers: { Authorization: `Bearer ${value}` },
    });
    if (!exchange.ok) throw new Error(`npm did not authorize this workflow for ${name}: HTTP ${exchange.status}`);
    assert.ok((await exchange.json()).token, `Missing npm OIDC token for ${name}`);
    console.log(`Verified trusted publisher for ${name}.`);
  }
}

const pending = [];
for (const pkg of packages) {
  assert.equal(pkg.file, `${pkg.name.slice(1).replace('/', '-')}-${version}.tgz`);
  const archive = join(output, pkg.file);
  const digest = createHash('sha512').update(await readFile(archive)).digest('base64');
  assert.equal(pkg.integrity, `sha512-${digest}`, `Damaged artifact: ${pkg.file}`);
  const published = await publishedIntegrity(pkg.name);
  if (published !== null) {
    assert.equal(published, pkg.integrity, `Different contents already published for ${pkg.name}@${version}`);
    console.log(`Already published and verified ${pkg.name}@${version}.`);
    continue;
  }
  // npm's implicit latest tag rejects outdated stable releases.
  const args = ['publish', archive, '--access', 'public', '--ignore-scripts', '--registry', registry];
  if (version.includes('-')) args.push('--tag', 'next');
  pending.push({ pkg, args });
}

if (process.env.GITHUB_ACTIONS === 'true') await verifyTrustedPublishers();
for (const { pkg, args } of pending) {
  await runNpm([...args, '--dry-run']);
  console.log(`Validated upload of ${pkg.name}@${version}.`);
}
if (live) {
  for (const { pkg, args } of pending) {
    await runNpm(args);
    let integrity = await publishedIntegrity(pkg.name);
    for (let retry = 0; integrity === null && retry < 6; retry++) {
      await delay(5000);
      integrity = await publishedIntegrity(pkg.name);
    }
    assert.equal(integrity, pkg.integrity, `Registry did not confirm the tested ${pkg.name}@${version} archive.`);
    console.log(`Published and verified ${pkg.name}@${version}.`);
  }
}
