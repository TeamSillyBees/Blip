'use strict';

const { spawn } = require('node:child_process');
const { constants } = require('node:os');
const { getTarget } = require('./platforms.cjs');
const { synchronize } = require('./sync.cjs');
const { version } = require('../package.json');

function resolveBinary(platform, arch, resolve = require.resolve) {
  const target = getTarget(platform, arch);
  try {
    return resolve(`${target.packageName}/bin/${target.binary}`);
  } catch (error) {
    if (error.code !== 'MODULE_NOT_FOUND') throw error;
    throw new Error(`Blip's native package ${target.packageName}@${version} is missing. Reinstall with optional dependencies enabled, for example: npm install --global @teamsillybees/blip@${version} --include=optional. Install on the machine where Blip will run; do not copy node_modules between operating systems or CPU architectures.`, { cause: error });
  }
}

// Inherit all terminal handles so the Go CLI receives real TTY input, resize
// events, and its own terminal capabilities for the interactive configuration UI.
function run(args, dependencies = {}) {
  const runtime = dependencies.process || process;
  const createChild = dependencies.spawn || spawn;
  let binary;
  try {
    const bundled = resolveBinary(runtime.platform, runtime.arch, dependencies.resolve);
    binary = (dependencies.synchronize || synchronize)(bundled, getTarget(runtime.platform, runtime.arch));
  } catch (error) {
    runtime.stderr.write(`Blip: ${error.message}\n`);
    runtime.exitCode = 1;
    return;
  }

  let child;
  try {
    child = createChild(binary, args, { stdio: 'inherit' });
  } catch (error) {
    runtime.stderr.write(`Blip: could not start ${binary}: ${error.message}\n`);
    runtime.exitCode = 1;
    return;
  }

  const listeners = new Map();
  for (const signal of ['SIGINT', 'SIGTERM', 'SIGHUP']) {
    const forward = () => {
      // Windows console Ctrl+C already reaches both attached processes. Node's
      // child.kill('SIGINT') uses forceful termination there and would prevent
      // the Go process from restoring its terminal and stopping gracefully.
      if (runtime.platform === 'win32' && signal === 'SIGINT') return;
      try { child.kill(signal); } catch { /* The child may already have exited. */ }
    };
    runtime.on(signal, forward);
    listeners.set(signal, forward);
  }
  const cleanup = () => {
    for (const [signal, listener] of listeners) runtime.removeListener(signal, listener);
  };

  let failed = false;
  child.once('error', (error) => {
    failed = true;
    cleanup();
    runtime.stderr.write(`Blip: could not start ${binary}: ${error.message}\n`);
    runtime.exitCode = 1;
  });
  child.once('exit', (code, signal) => {
    cleanup();
    if (failed) return;
    if (signal) {
      runtime.exitCode = 128 + (constants.signals[signal] || 1);
      // POSIX callers should observe the same terminating signal as the binary.
      // Windows has exit codes instead of POSIX signal termination semantics.
      if (runtime.platform !== 'win32') {
        try { runtime.kill(runtime.pid, signal); } catch { /* Use the exit code fallback. */ }
      }
    } else {
      runtime.exitCode = code === null ? 1 : code;
    }
  });
  return child;
}

module.exports = { resolveBinary, run };
