import { startRuntime } from './server.mjs';

const entry = process.argv[2] || '/srv/function/index.mjs';
if (!['/srv/function/index.mjs', '/srv/function/index.js'].includes(entry)) throw new Error('Invalid installed runtime entry');
const runtime = await startRuntime({ entry });
const stop = async () => {
  const timer = setTimeout(() => process.exit(1), 5000); timer.unref();
  await runtime.close(); process.exit(0);
};
process.once('SIGINT', stop);
process.once('SIGTERM', stop);
console.log(JSON.stringify({ component: 'function-runtime', state: 'ready', runtime: 'nodejs24' }));
