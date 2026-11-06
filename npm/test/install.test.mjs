import { test } from 'node:test';
import { smokeInstall } from '../../scripts/npm-install-smoke.mjs';

test('packed npm artifacts install and run without install scripts or public registry access', { timeout: 120_000 }, async () => {
  await smokeInstall();
});
