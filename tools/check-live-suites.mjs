// Keep the manually dispatched trusted Linux workflow's advertised suites
// executable. A missing shell case previously rejected Object Storage before
// the browser even started, despite the workflow offering it in the dropdown.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const root = fileURLToPath(new URL('../', import.meta.url));
const workflow = fs.readFileSync(path.join(root, '.github/workflows/live-e2e.yml'), 'utf8');
const advertised = workflow.match(/options: \[([^\]]+)\]/)?.[1].split(',').map((v) => v.trim());
assert(advertised?.length, 'Live UI workflow must advertise explicit suites');
assert.equal(new Set(advertised).size, advertised.length, 'Duplicate advertised suite');
const admission = workflow.match(/case "\$NEON_E2E_SUITE" in([\s\S]*?)esac/)?.[1];
assert(admission, 'Protected suite admission is required before credential preparation');
const accepted = [...admission.matchAll(/^\s*([a-z0-9.\-|]+)\)/gm)]
  .flatMap((v) => v[1].split('|'));
assert.deepEqual([...new Set(accepted)].sort(), [...advertised].sort(),
  'Every advertised suite must be admitted and every admitted suite advertised');
for (const suite of advertised) {
  assert(/^[a-z0-9-]+\.spec\.ts$/.test(suite), 'Only fixed test basenames are allowed');
  assert(fs.statSync(path.join(root, 'web/e2e', suite)).isFile(), 'Missing live suite: ' + suite);
}
const config = fs.readFileSync(path.join(root, 'web/playwright.config.ts'), 'utf8');
assert(/workers:\s*1\s*,/.test(config), 'Lab suites must be serial');
assert(/retries:\s*0\s*,/.test(config), 'Failures must remain visible instead of automatic retries');
console.log(JSON.stringify({ result: 'pass', advertised_suites: advertised.length, serial: true }));
