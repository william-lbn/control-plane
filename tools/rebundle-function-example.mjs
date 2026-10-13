// Reuse the locked, already built Linux pg dependency from a platform example.
// This intentionally supports only Stored ZIPs produced by our packaging tool.
// Customer bundles are validated by the Go runtime, never by this test utility.
import assert from 'node:assert/strict';
import { readFile, writeFile, mkdir, mkdtemp } from 'node:fs/promises';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
if (process.platform !== 'linux') throw new Error('Linux example packaging required');
const [input, entry, output] = process.argv.slice(2);
assert(input && entry && output, 'Usage: INPUT.zip ENTRY.mjs OUTPUT.zip');
const zip = await readFile(input);
assert(zip.length <= 8 * 1024 * 1024);
const directory = await mkdtemp(path.resolve(output) + '.source-');
let offset = 0, count = 0, size = 0;
const seen = new Set();
function crc32(bytes) { let crc=0xffffffff;for(const byte of bytes){crc^=byte;for(let bit=0;bit<8;bit++)crc=(crc>>>1)^(0xedb88320&-(crc&1));}return (crc^0xffffffff)>>>0; }
while (offset + 4 <= zip.length && zip.readUInt32LE(offset) === 0x04034b50) {
 assert(offset + 30 <= zip.length);
 const flags=zip.readUInt16LE(offset+6), method=zip.readUInt16LE(offset+8), crc=zip.readUInt32LE(offset+14), compressed=zip.readUInt32LE(offset+18), expanded=zip.readUInt32LE(offset+22), length=zip.readUInt16LE(offset+26), extra=zip.readUInt16LE(offset+28);
 assert(flags===0x800 && method===0 && compressed===expanded && extra===0);
 const start=offset+30+length, end=start+compressed;
 assert(end<=zip.length && ++count<=128 && (size+=expanded)<=16*1024*1024);
 const name=zip.subarray(offset+30,start).toString('utf8');
 assert(name && !name.includes('\\') && !name.includes('\0') && !path.posix.isAbsolute(name) && !name.split('/').some(part=>!part||part==='.'||part==='..') && !seen.has(name));
 seen.add(name);const body=zip.subarray(start,end);assert(crc32(body)===crc);
 const target=path.join(directory,name);await mkdir(path.dirname(target),{recursive:true,mode:0o700});await writeFile(target,body,{flag:'wx',mode:0o600});offset=end;
}
assert(seen.has('index.mjs') && offset+4<=zip.length && zip.readUInt32LE(offset)===0x02014b50);
await writeFile(path.join(directory,'index.mjs'),await readFile(entry),{mode:0o600});
execFileSync(process.execPath,[new URL('./package-function-example.mjs',import.meta.url).pathname,directory,output],{stdio:'inherit'});
