import assert from 'node:assert/strict';
import { test } from 'node:test';
import { splitBytes } from '../src/utils/bytes.ts';

test('字节单位在 B、KB、MB 边界正确切换', () => {
  assert.deepEqual(splitBytes(0), { value: 0, unit: 'B' });
  assert.deepEqual(splitBytes(1.5), { value: 1.5, unit: 'B' });
  assert.deepEqual(splitBytes(1023), { value: 1023, unit: 'B' });
  assert.deepEqual(splitBytes(1024), { value: 1, unit: 'KB' });
  assert.deepEqual(splitBytes(1536), { value: 1.5, unit: 'KB' });
  assert.deepEqual(splitBytes(1024 * 1024 - 1), { value: (1024 * 1024 - 1) / 1024, unit: 'KB' });
  assert.deepEqual(splitBytes(1024 * 1024), { value: 1, unit: 'MB' });
  assert.deepEqual(splitBytes(1024 ** 3), { value: 1024, unit: 'MB' });
});

test('负数和特殊值保持原有单位判断，精度交由页面处理', () => {
  assert.deepEqual(splitBytes(-1), { value: -1, unit: 'B' });
  assert.deepEqual(splitBytes(-Infinity), { value: -Infinity, unit: 'B' });
  assert.deepEqual(splitBytes(Infinity), { value: Infinity, unit: 'MB' });
  assert.deepEqual(splitBytes(NaN), { value: NaN, unit: 'MB' });
});
