'use strict';

const assert = require('node:assert/strict');
const { EventEmitter } = require('node:events');
const { test } = require('node:test');
const { targets, getTarget } = require('../blip/lib/platforms.cjs');
const { resolveBinary, run } = require('../blip/lib/launcher.cjs');
const manifest = require('../blip/package.json');

function fixture(platform = 'linux') {
  const runtime = new EventEmitter();
  Object.assign(runtime, { platform, arch: 'x64', pid: 4242, exitCode: undefined });
  let stderr = '';
  runtime.stderr = { write: (text) => { stderr += text; } };
  const kills = [];
  runtime.kill = (...args) => kills.push(args);
  const child = new EventEmitter();
  child.kill = (signal) => kills.push(['child', signal]);
  const calls = [];
  return {
    runtime, child, kills, calls,
    get stderr() { return stderr; },
    dependencies: {
      process: runtime,
      resolve: () => '/a directory with spaces/blip',
      spawn: (...args) => { calls.push(args); return child; },
    },
  };
}

test('every supported Node platform maps to its Go target and exact optional dependency', () => {
  assert.equal(targets.length, 6);
  for (const platform of ['darwin', 'linux', 'win32']) {
    for (const arch of ['x64', 'arm64']) {
      const target = getTarget(platform, arch);
      assert.equal(target.packageName, `@teamsillybees/blip-${platform}-${arch}`);
      assert.equal(target.goos, platform === 'win32' ? 'windows' : platform);
      assert.equal(target.goarch, arch === 'x64' ? 'amd64' : 'arm64');
      assert.equal(target.binary, platform === 'win32' ? 'blip.exe' : 'blip');
      assert.equal(manifest.optionalDependencies[target.packageName], manifest.version);
    }
  }
});

test('unsupported architectures fail with a useful supported-platform list', () => {
  assert.throws(() => getTarget('linux', 'ia32'), /linux\/ia32.*Supported platforms:.*linux-x64/);
  const state = fixture('freebsd');
  run([], state.dependencies);
  assert.equal(state.runtime.exitCode, 1);
  assert.equal(state.calls.length, 0);
  assert.match(state.stderr, /freebsd\/x64/);
});

test('binary resolution chooses the platform-specific executable', () => {
  let request;
  const binary = resolveBinary('win32', 'arm64', (name) => { request = name; return 'C:\\bin\\blip.exe'; });
  assert.equal(request, '@teamsillybees/blip-win32-arm64/bin/blip.exe');
  assert.equal(binary, 'C:\\bin\\blip.exe');
});

test('missing optional dependencies explain how to recover', () => {
  const state = fixture();
  state.dependencies.resolve = () => { throw Object.assign(new Error('missing'), { code: 'MODULE_NOT_FOUND' }); };
  run([], state.dependencies);
  assert.equal(state.runtime.exitCode, 1);
  assert.equal(state.calls.length, 0);
  assert.ok(state.stderr.includes(`@teamsillybees/blip-linux-x64@${manifest.version} is missing`));
  assert.match(state.stderr, /--include=optional/);
  assert.doesNotMatch(state.stderr, /at resolveBinary/);
});

test('passes arguments literally and inherits all terminal handles', () => {
  const state = fixture();
  const args = ['--config', 'a path with spaces/config.json', 'configure', '$HOME', ';echo nope'];
  assert.equal(run(args, state.dependencies), state.child);
  assert.deepEqual(state.calls, [['/a directory with spaces/blip', args, { stdio: 'inherit' }]]);
  state.child.emit('exit', 17, null);
  assert.equal(state.runtime.exitCode, 17);
  for (const signal of ['SIGINT', 'SIGTERM', 'SIGHUP']) assert.equal(state.runtime.listenerCount(signal), 0);
});

test('forwards termination signals and preserves POSIX signal termination', () => {
  const state = fixture();
  run([], state.dependencies);
  state.runtime.emit('SIGTERM');
  assert.deepEqual(state.kills, [['child', 'SIGTERM']]);
  state.child.emit('exit', null, 'SIGTERM');
  assert.deepEqual(state.kills, [['child', 'SIGTERM'], [4242, 'SIGTERM']]);
  assert.equal(state.runtime.exitCode, 143);
  assert.equal(state.runtime.listenerCount('SIGTERM'), 0);
});

test('Windows signal termination uses a nonzero exit code', () => {
  const state = fixture('win32');
  run([], state.dependencies);
  state.child.emit('exit', null, 'SIGINT');
  assert.equal(state.runtime.exitCode, 130);
  assert.deepEqual(state.kills, []);
});

test('Windows Ctrl+C lets the shared-console native process stop gracefully', () => {
  const state = fixture('win32');
  run([], state.dependencies);
  state.runtime.emit('SIGINT');
  assert.deepEqual(state.kills, [], 'Do not force-kill the child after it receives console Ctrl+C.');
  assert.equal(state.runtime.exitCode, undefined, 'Wait for the native CLI to finish its cleanup.');
  state.child.emit('exit', 0, null);
  assert.equal(state.runtime.exitCode, 0);
});

test('native spawn errors are concise and release signal handlers', () => {
  const state = fixture();
  run([], state.dependencies);
  state.child.emit('error', Object.assign(new Error('permission denied'), { code: 'EACCES' }));
  assert.equal(state.runtime.exitCode, 1);
  assert.match(state.stderr, /could not start .*permission denied/);
  assert.equal(state.runtime.listenerCount('SIGINT'), 0);
  state.child.emit('exit', 0, null);
  assert.equal(state.runtime.exitCode, 1);
});

test('synchronous spawn exceptions are reported without a stack trace', () => {
  const state = fixture();
  state.dependencies.spawn = () => { throw new Error('invalid executable'); };
  run([], state.dependencies);
  assert.equal(state.runtime.exitCode, 1);
  assert.match(state.stderr, /invalid executable/);
});
