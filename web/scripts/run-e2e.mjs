import { existsSync } from 'node:fs';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

for (const name of [
  'NEON_E2E_BASE_URL',
  'NEON_E2E_ADMIN_PASSWORD_FILE',
  'NEON_E2E_PRIVATE_DIR',
  'NEON_E2E_ARTIFACTS',
]) {
  if (!process.env[name]) throw new Error('Required live test setting: ' + name);
}
const output = path.resolve(process.env.NEON_E2E_ARTIFACTS);
if (existsSync(output))
  throw new Error('Evidence directory already exists; choose a new attempt and retain its records');
const cli = fileURLToPath(new URL('../node_modules/@playwright/test/cli.js', import.meta.url));
const result = spawnSync(process.execPath, [cli, 'test', ...process.argv.slice(2)], {
  stdio: 'inherit',
});
if (result.error) throw result.error;
process.exit(result.status ?? 1);
