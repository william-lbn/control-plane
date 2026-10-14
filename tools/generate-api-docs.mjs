// Keep the human operation index tied to the shipped machine contract.
// --check is read-only and used by Linux CI; generation changes only API.md.
import fs from 'node:fs';
const root=new URL('../',import.meta.url);
const spec=JSON.parse(fs.readFileSync(new URL('contracts/openapi-v1.json',root),'utf8'));
const file=new URL('docs/API.md',root);
const before=fs.readFileSync(file,'utf8').replaceAll('\r\n','\n');
const methods=['get','head','post','put','patch','delete','options'];
const rows=[];
for(const [path,item] of Object.entries(spec.paths))for(const method of methods){
 const operation=item[method];if(!operation)continue;
 if(!operation.operationId)throw new Error('Operation ID missing');
 const statuses=Object.keys(operation.responses).filter(status=>/^2[0-9]{2}$/.test(status));
 rows.push(`| ${method.toUpperCase()} | \`${path}\` | \`${operation.operationId}\` | ${statuses.join(', ')} |`);
}
const heading='## 2. 操作索引\n';
const start=before.indexOf(heading),end=before.indexOf('\n## 3.',start);
if(start<0||end<0)throw new Error('API index boundaries missing');
let after=before.slice(0,start)+heading+'\n以下索引由 `tools/generate-api-docs.mjs` 从实际 OpenAPI 生成，Linux CI 检查同步。\n\n| Method | Path | operationId | Success responses |\n| --- | --- | --- | --- |\n'+rows.join('\n')+'\n'+before.slice(end);
after=after.replace(/^版本 .* 个路径、.* 个操作，全部拥有稳定且唯一的 operationId。$/m,`版本 ${spec.info.version}，${Object.keys(spec.paths).length} 个路径、${rows.length} 个操作，全部拥有稳定且唯一的 operationId。`);
if(process.argv.slice(2).some(v=>v!=='--check'))throw new Error('Only --check is supported');
if(process.argv.includes('--check')){if(after!==before)throw new Error('API.md differs; run node tools/generate-api-docs.mjs');}
else fs.writeFileSync(file,after);
console.log(JSON.stringify({result:'pass',version:spec.info.version,paths:Object.keys(spec.paths).length,operations:rows.length}));
