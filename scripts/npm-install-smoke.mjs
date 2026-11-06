import assert from 'node:assert/strict';
import { mkdir, mkdtemp, readFile, realpath, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join, relative, resolve, sep } from 'node:path';
import { createRequire } from 'node:module';
import { fileURLToPath, pathToFileURL } from 'node:url';
import platforms from '../npm/blip/lib/platforms.cjs';
import { run, runNpm } from './npm-helpers.mjs';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');

export async function smokeInstall() {
  const output = join(root, 'dist', 'npm');
  let manifest;
  try {
    manifest = JSON.parse(await readFile(join(output, 'manifest.json'), 'utf8'));
  } catch (error) {
    throw new Error('Build the npm packages first: npm run build:npm:current', { cause: error });
  }
  const target = platforms.getTarget();
  const native = manifest.platforms.find((item) => item.id === target.id);
  assert.ok(native, `Build ${target.id} first: npm run build:npm:current`);
  // Node resolves installed modules through real paths. macOS's temporary
  // directory can be reached through /var -> /private/var, so use the same root
  // for installation containment and cleanup checks.
  const tempRoot = await realpath(tmpdir());
  const temp = await mkdtemp(join(tempRoot, 'blip npm smoke '));
  try {
    const userConfig = join(temp, 'user.npmrc');
    const globalConfig = join(temp, 'global.npmrc');
    await writeFile(userConfig, '');
    await writeFile(globalConfig, '');
    const userHome = join(temp, 'home');
    await mkdir(userHome);
    const env = Object.fromEntries(Object.entries(process.env).filter(([key]) => !/^npm_config_/i.test(key)));
    Object.assign(env, {
      HOME: userHome,
      USERPROFILE: userHome,
      APPDATA: join(userHome, 'AppData', 'Roaming'),
      LOCALAPPDATA: join(userHome, 'AppData', 'Local'),
      XDG_CONFIG_HOME: join(userHome, '.config'),
      XDG_DATA_HOME: join(userHome, '.local', 'share'),
      CODEX_HOME: join(userHome, '.codex'),
      NPM_CONFIG_UPDATE_NOTIFIER: 'false',
    });
    const common = [
      '--global', '--registry', 'http://127.0.0.1:9/',
      '--userconfig', userConfig, '--globalconfig', globalConfig,
      '--cache', join(temp, 'cache'), '--no-audit', '--no-fund',
      '--ignore-scripts', '--offline', '--fetch-retries=0', '--fetch-timeout=5000',
    ];
    const prefix = join(temp, 'installed');
    await runNpm(['install', ...common, '--prefix', prefix, '--include=optional', join(output, manifest.wrapper.file), join(output, native.file)], { cwd: temp, env });
    const moduleRoot = process.platform === 'win32' ? join(prefix, 'node_modules') : join(prefix, 'lib', 'node_modules');
    const wrapperDirectory = join(moduleRoot, '@teamsillybees', 'blip');
    const shim = join(wrapperDirectory, 'bin', 'blip.cjs');
    const installedRequire = createRequire(join(wrapperDirectory, 'package.json'));
    const binary = installedRequire.resolve(`${target.packageName}/bin/${target.binary}`);
    assert.ok(binary.startsWith(prefix + sep), 'The binary must resolve inside the temporary installation prefix.');
    const nativeManifest = installedRequire(`${target.packageName}/package.json`);
    assert.equal(nativeManifest.version, manifest.version);
    const invoke = (args) => run(process.execPath, [shim, ...args], { cwd: temp, env });
    const version = await invoke(['version']);
    assert.equal(version.code, 0, version.stderr);
    assert.equal(version.stdout.trim(), manifest.version);
    const help = await invoke(['--help']);
    assert.equal(help.code, 0, help.stderr);
    assert.match(help.stdout, /configure/);
    const config = join(temp, 'configuration with spaces', 'config.json');
    const init = await invoke(['--config', config, 'init']);
    assert.equal(init.code, 0, init.stderr);
    const initialConfig = JSON.parse(await readFile(config, 'utf8'));
    assert.ok(initialConfig.destinations.every((destination) => destination.enabled === false));
    const invalid = await invoke(['definitely-not-a-command']);
    assert.notEqual(invalid.code, 0);
    assert.match(invalid.stderr, /unknown command/i);
    // npm's generated bin entry must exist and be executable too. Windows' .cmd
    // shim is exercised by cmd.exe with a fixed command from a controlled cwd.
    const linked = process.platform === 'win32'
      ? await run(process.env.ComSpec || 'cmd.exe', ['/d', '/s', '/c', 'blip.cmd version'], { cwd: prefix, env })
      : await run(join(prefix, 'bin', 'blip'), ['version'], { cwd: temp, env });
    assert.equal(linked.code, 0, linked.stderr);
    assert.equal(linked.stdout.trim(), manifest.version);

    const missingPrefix = join(temp, 'without-optional');
    await runNpm(['install', ...common, '--prefix', missingPrefix, '--omit=optional', join(output, manifest.wrapper.file)], { cwd: temp, env });
    const missingModules = process.platform === 'win32' ? join(missingPrefix, 'node_modules') : join(missingPrefix, 'lib', 'node_modules');
    const missing = await run(process.execPath, [join(missingModules, '@teamsillybees', 'blip', 'bin', 'blip.cjs'), 'version'], { cwd: temp, env });
    assert.equal(missing.code, 1);
    assert.match(missing.stderr, /native package.*is missing/);
    assert.match(missing.stderr, /--include=optional/);
    assert.doesNotMatch(missing.stderr, /MODULE_NOT_FOUND|Require stack:/);
    return { version: manifest.version, platform: target.id, checks: ['offline local tarball install', 'native resolution', 'version', 'help', 'init with spaces', 'nonzero exit', 'npm bin entry', 'missing optional dependency'] };
  } finally {
    const contained = relative(tempRoot, resolve(temp));
    if (!contained || contained === '..' || contained.startsWith(`..${sep}`) || !contained.startsWith('blip npm smoke ')) {
      throw new Error('Refusing to remove smoke-test files outside the temporary directory.');
    }
    await rm(temp, { recursive: true, force: true, maxRetries: 3, retryDelay: 100 });
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  smokeInstall().then((result) => {
    process.stdout.write(`npm install smoke passed: ${result.platform} ${result.version} (${result.checks.join(', ')})\n`);
  }).catch((error) => {
    process.stderr.write(`${error.stack || error}\n`);
    process.exitCode = 1;
  });
}
