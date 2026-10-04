import { copyFileSync, mkdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = join(dirname(fileURLToPath(import.meta.url)), '..');
const source = join(root, 'node_modules', 'swagger-ui-dist');
const dest = join(root, 'dist', 'api', 'docs', 'assets');
mkdirSync(dest, { recursive: true });
for (const name of [
  'swagger-ui-bundle.js',
  'swagger-ui-standalone-preset.js',
  'swagger-ui.css',
  'favicon-32x32.png',
]) {
  copyFileSync(join(source, name), join(dest, name));
}
copyFileSync(join(root, 'scripts', 'swagger-init.js'), join(dest, 'swagger-init.js'));
