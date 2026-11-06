import { existsSync } from 'node:fs';
import { delimiter, dirname, join } from 'node:path';
import { spawn } from 'node:child_process';

export function npmCommand(args, env = process.env) {
  // npm.cmd requires cmd.exe on Windows. Invoke npm's JS entry point directly
  // instead, so paths and arguments never pass through a shell.
  const candidates = [env.npm_execpath, join(dirname(process.execPath), 'node_modules', 'npm', 'bin', 'npm-cli.js')];
  for (const directory of (env.PATH || env.Path || '').split(delimiter)) {
    candidates.push(join(directory, 'node_modules', 'npm', 'bin', 'npm-cli.js'));
  }
  const cli = candidates.find((candidate) => candidate && /\.m?js$/i.test(candidate) && existsSync(candidate));
  if (cli) return { command: process.execPath, args: [cli, ...args] };
  if (process.platform !== 'win32') return { command: 'npm', args };
  throw new Error('Cannot locate npm-cli.js. Run this command through npm run, or install npm alongside Node.js.');
}

export function run(command, args, options = {}) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, { ...options, stdio: ['ignore', 'pipe', 'pipe'] });
    let stdout = '';
    let stderr = '';
    child.stdout.setEncoding('utf8');
    child.stderr.setEncoding('utf8');
    child.stdout.on('data', (chunk) => { stdout += chunk; });
    child.stderr.on('data', (chunk) => { stderr += chunk; });
    child.once('error', reject);
    child.once('close', (code, signal) => resolve({ code, signal, stdout, stderr }));
  });
}

export async function checkedRun(command, args, options) {
  const result = await run(command, args, options);
  if (result.code !== 0) {
    throw new Error(`${command} exited with ${result.signal || result.code}\n${result.stderr || result.stdout}`);
  }
  return result;
}

export function runNpm(args, options = {}) {
  const npm = npmCommand(args);
  return checkedRun(npm.command, npm.args, options);
}
