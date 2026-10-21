// Deterministic acceptance fixture only; never run or import customer code.
import assert from 'node:assert/strict';
import {readFile,writeFile} from 'node:fs/promises';
import {createHash} from 'node:crypto';
if(process.platform!=='linux') throw Error('Linux fixture generation required');
const [input,output]=process.argv.slice(2);
assert(input&&output);
const source=await readFile(input,'utf8');
assert.equal(source.split('branch-copy-v1').length,2,'Exact v1 fixture required');
const variant=source.replace('branch-copy-v1','branch-copy-v2');
await writeFile(output,variant,{flag:'wx',mode:0o600});
console.log(JSON.stringify({branch_copy_fixture_variant:'v2',source_sha256:createHash('sha256').update(source).digest('hex'),variant_sha256:createHash('sha256').update(variant).digest('hex'),customer_code_executed:false}));
