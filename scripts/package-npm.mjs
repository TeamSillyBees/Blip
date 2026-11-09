import { chmod, copyFile, cp, mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { dirname, join, relative, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createHash } from 'node:crypto';
import platforms from '../npm/blip/lib/platforms.cjs';
import { checkedRun, runNpm } from './npm-helpers.mjs';
import { executableArchive } from './npm-archive.mjs';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const output = join(root, 'dist', 'npm');
const wrapperSource = join(root, 'npm', 'blip');
const wrapper = JSON.parse(await readFile(join(wrapperSource, 'package.json'), 'utf8'));
const development = JSON.parse(await readFile(join(root, 'package.json'), 'utf8'));

function selectedTargets(args) {
  if (args.length === 0) return platforms.targets;
  if (args.length !== 2 || args[0] !== '--target') {
    throw new Error(`Usage: npm run build:npm -- [--target current|${platforms.targets.map((target) => target.id).join('|')}]`);
  }
  const id = args[1] === 'current' ? platforms.getTarget().id : args[1];
  const target = platforms.targets.find((item) => item.id === id);
  if (!target) throw new Error(`Unsupported target: ${id}`);
  return [target];
}

async function addNotices(directory) {
  await copyFile(join(root, 'README.md'), join(directory, 'README.md'));
  await copyFile(join(root, 'THIRD_PARTY_NOTICES.md'), join(directory, 'THIRD_PARTY_NOTICES.md'));
}

async function pack(directory, executable) {
  const result = await runNpm(['pack', directory, '--pack-destination', output, '--ignore-scripts', '--json'], { cwd: root });
  const [packed] = JSON.parse(result.stdout);
  const archivePath = join(output, packed.filename);
  const archive = executableArchive(await readFile(archivePath), `package/bin/${executable}`);
  await writeFile(archivePath, archive);
  return {
    name: packed.name,
    file: packed.filename,
    integrity: `sha512-${createHash('sha512').update(archive).digest('base64')}`,
    shasum: createHash('sha1').update(archive).digest('hex'),
  };
}

async function main() {
  const targets = selectedTargets(process.argv.slice(2));
  if (development.version !== wrapper.version) throw new Error('Root and npm/blip/package.json versions must match.');
  if (Object.keys(wrapper.optionalDependencies).length !== platforms.targets.length || platforms.targets.some((target) => wrapper.optionalDependencies[target.packageName] !== wrapper.version)) {
    throw new Error('Every native optional dependency must match the wrapper version.');
  }
  await mkdir(output, { recursive: true });
  const staging = await mkdtemp(join(output, '.staging-'));
  try {
    const manifest = { version: wrapper.version, wrapper: null, platforms: [] };
    for (const target of targets) {
      process.stdout.write(`Building ${target.id}...\n`);
      const directory = join(staging, target.id);
      await mkdir(join(directory, 'bin'), { recursive: true });
      const binary = join(directory, 'bin', target.binary);
      await checkedRun('go', ['build', '-mod=readonly', '-trimpath', '-buildvcs=false', '-ldflags', `-s -w -X main.version=${wrapper.version}`, '-o', binary, './cmd/blip'], {
        cwd: root,
        env: { ...process.env, CGO_ENABLED: '0', GOOS: target.goos, GOARCH: target.goarch },
      });
      await chmod(binary, 0o755);
      await writeFile(join(directory, 'package.json'), `${JSON.stringify({
        name: target.packageName,
        version: wrapper.version,
        description: `Blip native binary for ${target.platform} ${target.arch}`,
        os: [target.platform],
        cpu: [target.arch],
        files: ['bin', 'THIRD_PARTY_NOTICES.md'],
        repository: wrapper.repository,
        homepage: wrapper.homepage,
        publishConfig: wrapper.publishConfig,
      }, null, 2)}\n`);
      await addNotices(directory);
      manifest.platforms.push({ ...target, ...await pack(directory, target.binary) });
    }
    const wrapperDirectory = join(staging, 'wrapper');
    await cp(wrapperSource, wrapperDirectory, { recursive: true });
    await chmod(join(wrapperDirectory, 'bin', 'blip.cjs'), 0o755);
    await addNotices(wrapperDirectory);
    manifest.wrapper = await pack(wrapperDirectory, 'blip.cjs');
    await writeFile(join(output, 'manifest.json'), `${JSON.stringify(manifest, null, 2)}\n`);
    process.stdout.write(`Packed ${manifest.platforms.length + 1} packages in ${output}\n`);
  } finally {
    // Only delete the unique staging directory created beneath dist/npm above.
    const contained = relative(output, resolve(staging));
    if (!contained || contained === '..' || contained.startsWith(`..${sep}`)) throw new Error('Refusing to remove staging outside dist/npm.');
    await rm(staging, { recursive: true, force: true });
  }
}

main().catch((error) => {
  process.stderr.write(`${error.message}\n`);
  process.exitCode = 1;
});
