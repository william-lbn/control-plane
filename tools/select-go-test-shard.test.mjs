import test from 'node:test';
import assert from 'node:assert/strict';
import {selectGoTestShard} from './select-go-test-shard.mjs';

test('six shards cover all discovered tests exactly once', () => {
  const names = Array.from({length: 120}, (_, i) => 'TestScenario' + i);
  const source = names.join('\n') + '\nok\tmodule\t0.2s\n';
  const shards = Array.from({length: 6}, (_, i) => selectGoTestShard(source, i, 6));
  const combined = shards.flatMap((s) => s.tests);
  assert.equal(combined.length, names.length);
  assert.equal(new Set(combined).size, names.length);
  assert.deepEqual(combined.sort(), names.sort());
  assert.equal(selectGoTestShard(source, 2, 6).pattern, shards[2].pattern);
});

test('reject duplicate inventory and invalid index/count', () => {
  assert.throws(() => selectGoTestShard('TestOne\nTestOne\n', 0, 1));
  assert.throws(() => selectGoTestShard('ok\tmodule', 0, 1));
  assert.throws(() => selectGoTestShard('TestOne', 1, 1));
  assert.throws(() => selectGoTestShard('TestOne', 0, 0));
  assert.equal(selectGoTestShard('TestOne', 0, 1).pattern, '^(TestOne)$');
});
