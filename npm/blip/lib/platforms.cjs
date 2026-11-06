'use strict';

const targets = Object.freeze([
  { platform: 'darwin', arch: 'arm64', goos: 'darwin', goarch: 'arm64' },
  { platform: 'darwin', arch: 'x64', goos: 'darwin', goarch: 'amd64' },
  { platform: 'linux', arch: 'arm64', goos: 'linux', goarch: 'arm64' },
  { platform: 'linux', arch: 'x64', goos: 'linux', goarch: 'amd64' },
  { platform: 'win32', arch: 'arm64', goos: 'windows', goarch: 'arm64' },
  { platform: 'win32', arch: 'x64', goos: 'windows', goarch: 'amd64' },
].map((target) => Object.freeze({
  ...target,
  id: `${target.platform}-${target.arch}`,
  packageName: `@teamsillybees/blip-${target.platform}-${target.arch}`,
  binary: target.platform === 'win32' ? 'blip.exe' : 'blip',
})));

function getTarget(platform = process.platform, arch = process.arch) {
  const target = targets.find((item) => item.platform === platform && item.arch === arch);
  if (!target) {
    throw new Error(`Blip does not provide an npm binary for ${platform}/${arch}. Supported platforms: ${targets.map((item) => item.id).join(', ')}.`);
  }
  return target;
}

module.exports = { targets, getTarget };
