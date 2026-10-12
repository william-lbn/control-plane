// Linux-only reproducible example ZIP. Copy installed production JS/JSON files
// directly; no untrusted npm build script, runtime compiler or customer execution.
import { readdir, readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';
if (process.platform !== 'linux') throw new Error('Build the example on Linux');
const [source, output]=process.argv.slice(2);
if (!source||!output) throw new Error('Usage: node tools/package-function-example.mjs SOURCE OUTPUT.zip');
const files=[];
async function collect(directory,relative='') {
 for (const entry of await readdir(directory,{withFileTypes:true})) {
  if (entry.name.startsWith('.') || /^(test|tests|docs|examples|bench|benchmark|types)$/.test(entry.name)) continue;
  const name=path.posix.join(relative,entry.name);
  if (entry.isSymbolicLink()) throw new Error('Example symlink rejected');
  if (entry.isDirectory()) await collect(path.join(directory,entry.name),name);
  else if (/\.(js|mjs|cjs|json)$/.test(entry.name) || /^(licen[cs]e|notice)(\..*)?$/i.test(entry.name)) files.push({name,bytes:await readFile(path.join(directory,entry.name))});
 }
}
await collect(path.resolve(source));
if (!files.some(f=>f.name==='LICENSE')) files.push({name:'LICENSE',bytes:await readFile(new URL('../LICENSE',import.meta.url))});
files.sort((a,b)=>a.name.localeCompare(b.name,'en'));
if (files.length>128 || !files.some(f=>f.name==='index.mjs')) throw new Error('Example file budget or entry invalid');
function crc32(bytes) { let crc=0xffffffff;for (const b of bytes) {crc^=b;for(let bit=0;bit<8;bit++)crc=(crc>>>1)^(0xedb88320&-(crc&1));}return (crc^0xffffffff)>>>0; }
const locals=[],centrals=[];let offset=0;
for (const file of files) {
 const name=Buffer.from(file.name);const crc=crc32(file.bytes);const h=Buffer.alloc(30);
 h.writeUInt32LE(0x04034b50);h.writeUInt16LE(20,4);h.writeUInt16LE(0x800,6);h.writeUInt16LE(33,12);h.writeUInt32LE(crc,14);h.writeUInt32LE(file.bytes.length,18);h.writeUInt32LE(file.bytes.length,22);h.writeUInt16LE(name.length,26);
 locals.push(h,name,file.bytes);const c=Buffer.alloc(46);c.writeUInt32LE(0x02014b50);c.writeUInt16LE(20,4);c.writeUInt16LE(20,6);c.writeUInt16LE(0x800,8);c.writeUInt16LE(33,14);c.writeUInt32LE(crc,16);c.writeUInt32LE(file.bytes.length,20);c.writeUInt32LE(file.bytes.length,24);c.writeUInt16LE(name.length,28);c.writeUInt32LE(offset,42);centrals.push(c,name);offset+=h.length+name.length+file.bytes.length;
}
const central=Buffer.concat(centrals);const end=Buffer.alloc(22);end.writeUInt32LE(0x06054b50);end.writeUInt16LE(files.length,8);end.writeUInt16LE(files.length,10);end.writeUInt32LE(central.length,12);end.writeUInt32LE(offset,16);
const zip=Buffer.concat([...locals,central,end]);if(zip.length>8*1024*1024) throw new Error('Example exceeds deployment budget');
await writeFile(output,zip,{flag:'wx',mode:0o600});console.log(JSON.stringify({files:files.length,bytes:zip.length}));
