import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';

const files = execFileSync('git', ['ls-files', '-z'], { encoding: 'utf8' }).split('\0').filter(Boolean);
const forbidden = /(^|\/)(\.local|node_modules|evidence|dist|bin|artifacts|test-results|playwright-report)(\/|$)|\.(py|ps1|pem|key|dump|db|sqlite|etl|zip)$|(^|\/)\.env($|\.)/;
const suspicious = /-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----|\bsk-(?:proj-|svcacct-)[A-Za-z0-9_-]{20,}/;
const violations = [];
for (const file of files) {
  if (forbidden.test(file) || suspicious.test(readFileSync(file, 'utf8'))) violations.push(file);
}
if (files.length === 0 || violations.length) {
  throw new Error('Public source gate failed: ' + violations.join(', '));
}
console.log('Public source gate: ' + files.length + ' files; no excluded lab/Python/private material');
