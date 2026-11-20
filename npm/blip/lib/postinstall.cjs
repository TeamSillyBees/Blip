'use strict';

const { resolveBinary } = require('./launcher.cjs');
const { synchronize } = require('./sync.cjs');

if (process.env.npm_config_global === 'true' || process.env.npm_config_location === 'global') {
  try {
    synchronize(resolveBinary(process.platform, process.arch));
  } catch (error) {
    process.stderr.write(`Blip: ${error.message}\n`);
    process.exitCode = 1;
  }
}
