// 将当前 Go 测试清单稳定分配给 CI 分片，不依赖历史失败名单。
// SHA256(name) 取高位决定所属分片；任何新增测试会自动进入恰好一片。
import {createHash} from 'node:crypto';
import {readFileSync, writeFileSync} from 'node:fs';
import {pathToFileURL} from 'node:url';

export function selectGoTestShard(raw, index, count) {
  if (!Number.isSafeInteger(index) || index < 0 ||
      !Number.isSafeInteger(count) || count < 1 || count > 64 || index >= count) {
    throw new Error('invalid index or shard count');
  }
  const tests = raw.split(/\r?\n/).filter((name) => /^Test[A-Za-z0-9_]+$/.test(name)).sort();
  if (!tests.length || tests.length !== new Set(tests).size) {
    throw new Error('duplicate or empty test inventory');
  }
  const shardOf = (name) => createHash('sha256').update(name).digest().readUInt32BE(0) % count;
  const selected = tests.filter((name) => shardOf(name) === index);
  if (!selected.length) throw new Error('empty test shard');
  return {
    version: 1,
    index, count, total: tests.length,
    tests: selected,
    pattern: '^(' + selected.join('|') + ')$',
    inventory_sha256: createHash('sha256').update(tests.join('\n')).digest('hex'),
  };
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const [input, index, count, output] = process.argv.slice(2);
  if (!input || !output || !/^\d+$/.test(index ?? '') || !/^\d+$/.test(count ?? '')) {
    throw new Error('usage: select-go-test-shard.mjs input index count output');
  }
  const result = selectGoTestShard(readFileSync(input, 'utf8'), Number(index), Number(count));
  if (result.total < 500) throw new Error('unexpectedly small control-plane test inventory');
  writeFileSync(output, JSON.stringify(result, null, 2) + '\n', {flag: 'wx', mode: 0o600});
  console.log('CI shard ' + result.index + '/' + result.count + ' tests=' + result.tests.length + '/' + result.total);
}
