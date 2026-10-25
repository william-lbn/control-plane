#!/usr/bin/env node
import { pathToFileURL } from 'node:url';
import { search, operationDetail } from './catalog.mjs';
import { loadProfile, ControlClient } from './client.mjs';

const usage = 'neon-control search [--query text --method GET --tag name --cursor value --limit 25]\n' +
  'neon-control describe <operation_id>\n' +
  'neon-control call <operation_id> [--path JSON --query JSON --headers JSON --body JSON --body-file path --content-type type --output-file new-path --write]\n' +
  'API credentials and fixed origin are read only from NEON_CONTROL_PROFILE. No automatic retry is sent.';

function parse(args) {
  const command = args.shift(), id = ['describe','call'].includes(command) ? args.shift() : null;
  const options = {};
  while (args.length) {
    const name = args.shift();
    if (typeof name !== 'string' || !name.startsWith('--') || Object.hasOwn(options, name)) throw new Error('Unknown or duplicate option');
    if (name === '--write') { options[name] = true; continue; }
    const value = args.shift();
    if (value === undefined || value.length > 1024 * 1024) throw new Error('Missing or excessive option value');
    options[name] = value;
  }
  return { command, id, options };
}
export async function main(argv = process.argv.slice(2)) {
  if (!argv.length || argv[0] === '--help') { process.stdout.write(usage + '\n'); return; }
  const { command, id, options } = parse([...argv]);
  const permitted = command === 'search' ? ['--query','--method','--tag','--cursor','--limit'] : command === 'call' ? ['--path','--query','--headers','--body','--body-file','--content-type','--output-file','--write'] : [];
  if (Object.keys(options).some(key => !permitted.includes(key))) throw new Error('Option is not supported by this command');
  let value;
  if (command === 'search') value = search({ query: options['--query'], method: options['--method'], tag: options['--tag'], cursor: options['--cursor'], limit: options['--limit'] === undefined ? 25 : Number(options['--limit']) });
  else if (command === 'describe') value = operationDetail(id);
  else if (command === 'call') {
    const input = {};
    for (const key of ['path','query','headers','body']) if (options['--' + key] !== undefined) {
      try { input[key] = JSON.parse(options['--' + key]); } catch { throw new Error('Invalid JSON in --' + key); }
    }
    if (options['--content-type']) input.content_type = options['--content-type'];
    const client = new ControlClient(loadProfile());
    try { value = await client.execute(id, input, { allowWrite: options['--write'] === true, bodyFile: options['--body-file'], outputFile: options['--output-file'] }); }
    finally { client.close(); }
    if (value.status >= 400) process.exitCode = 2;
  } else throw new Error('Unknown command; use --help');
  process.stdout.write(JSON.stringify(value) + '\n');
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch(error => { process.stderr.write((error instanceof Error ? error.message : 'Control client failed') + '\n'); process.exitCode = 1; });
}
