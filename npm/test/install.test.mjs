import { test } from 'node:test';
import { smokeInstall } from '../../scripts/npm-install-smoke.mjs';

test('offline global install and first launch synchronize the verified fixed executable', { timeout: 120_000 }, async () => {
  await smokeInstall();
});
