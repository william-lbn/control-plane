import { readFileSync } from 'node:fs';
const { images } = JSON.parse(readFileSync(new URL('../containers/builders.lock.json', import.meta.url), 'utf8'));
for (const [name, value] of Object.entries({ GO_BUILDER_IMAGE: images.go, NODE_BUILDER_IMAGE: images.node, WEB_RUNTIME_IMAGE: images.web })) {
  if (!/^[a-z0-9./-]+@sha256:[a-f0-9]{64}$/.test(value)) throw new Error(`Unpinned builder: ${name}`);
  console.log(`${name}=${value}`);
}
