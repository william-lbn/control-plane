// Install only explicitly reviewed runtime files into a NEW offline disk copy.
import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import { spawnSync } from 'node:child_process';

const [tree, disk, output] = process.argv.slice(2);
if (process.platform !== 'linux' || !tree || !disk || !output || [tree, disk, output].some(p => !/^\/[a-zA-Z0-9/_.-]+$/.test(p))) throw new Error('Explicit safe absolute Linux paths required');
if (!fs.statSync(disk).isFile() || fs.existsSync(output)) throw new Error('New runtime verification output required');
fs.mkdirSync(output, { mode: 0o700 });
const hash = bytes => crypto.createHash('sha256').update(bytes).digest('hex');
function debug(command) {
  const result = spawnSync('debugfs', ['-w', '-R', command, disk], { encoding: 'utf8' });
  fs.appendFileSync(path.join(output, 'debugfs.log'), command + '\n' + result.stdout + result.stderr);
  if (result.status !== 0) throw new Error('Offline disk operation failed');
  return result.stdout;
}
const sourcePackage = JSON.parse(fs.readFileSync(path.join(tree, 'package.json')));
const wsPackage = JSON.parse(fs.readFileSync(path.join(tree, 'node_modules/ws/package.json')));
if (sourcePackage.dependencies.ws !== wsPackage.version || wsPackage.version !== '8.22.0') throw new Error('Pinned installed WebSocket package required');
const files = [];
function walk(relative) {
  const local = path.join(tree, relative), stat = fs.lstatSync(local);
  if (stat.isSymbolicLink() || !/^[a-zA-Z0-9/_.-]+$/.test(relative)) throw new Error('Unsafe runtime tree');
  if (stat.isDirectory()) { for (const name of fs.readdirSync(local).sort()) walk(relative + '/' + name); }
  else if (stat.isFile()) files.push(relative);
  else throw new Error('Only ordinary runtime files allowed');
}
for (const item of ['runtime', 'node_modules/ws', 'package.json', 'package-lock.json']) walk(item);
if (files.length > 100 || !files.includes('runtime/websocket.mjs') || files.some(f => fs.statSync(path.join(tree, f)).size > 2 ** 20)) throw new Error('Runtime tree bounds exceeded');
const folders = new Set(['/opt/neon/functions']);
for (const f of files) for (let d = path.posix.dirname('/opt/neon/functions/' + f); d !== '/'; d = path.posix.dirname(d)) folders.add(d);
for (const folder of [...folders].sort((a, b) => a.length - b.length)) {
  if (!debug('stat ' + folder).includes('Inode:')) debug('mkdir ' + folder);
  debug('set_inode_field ' + folder + ' mode 040755');
}
const manifest = [];
for (const [index, relative] of files.entries()) {
  const target = '/opt/neon/functions/' + relative, local = path.join(tree, relative), extracted = path.join(output, 'file-' + index);
  if (debug('stat ' + target).includes('Inode:')) debug('rm ' + target);
  debug('write ' + local + ' ' + target);
  for (const [field, value] of [['mode', '0100644'], ['uid', '0'], ['gid', '0']]) debug(`set_inode_field ${target} ${field} ${value}`);
  debug('dump ' + target + ' ' + extracted);
  const sha = hash(fs.readFileSync(local));
  if (hash(fs.readFileSync(extracted)) !== sha || !/Mode:\s+0644/.test(debug('stat ' + target))) throw new Error('Runtime readback failed');
  manifest.push({ path: target, sha256: sha, mode: '0644', uid: 0, gid: 0 });
}
fs.writeFileSync(path.join(output, 'result.json'), JSON.stringify({ result: 'pass', files: manifest, runtime_tree_sha256: hash(JSON.stringify(manifest)), ws_version: wsPackage.version, parent_node_binary_retained: true }, null, 2) + '\n');
console.log(JSON.stringify({ result: 'pass', installed_runtime_files: files.length }));
