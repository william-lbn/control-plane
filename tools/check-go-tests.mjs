import { readFileSync, writeFileSync } from 'node:fs';

const events = readFileSync(process.argv[2], 'utf8').trim().split('\n').map((line) => JSON.parse(line));
const tests = (action) => events.filter((entry) => entry.Action === action && entry.Test).map((entry) => entry.Test);
const failed = events.filter((entry) => entry.Action === 'fail').map((entry) => entry.Test ?? entry.Package);
const skipped = tests('skip');
const passed = tests('pass');
const result = { result: failed.length || skipped.length || !passed.length ? 'fail' : 'pass', passed, failed, skipped };
writeFileSync(process.argv[3], JSON.stringify(result, null, 2) + '\n');
if (result.result !== 'pass') throw new Error('Go CI requires completed passing tests and zero skips; inspect the report');
console.log(`Go CI: ${passed.length} pass events, zero failed or skipped tests`);
