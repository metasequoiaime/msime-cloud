import assert from 'node:assert/strict';
import { test } from 'node:test';
import { formatHours } from '../src/utils/duration.ts';

test('处理时长默认使用小时，保留小数和整数舍入边界', () => {
  for (const [value, expected] of [
    [null, '—'],
    [0, '0.0 小时'],
    [0.5, '0.5 小时'],
    [1, '1.0 小时'],
    [2.54, '2.5 小时'],
    [9.96, '10.0 小时'],
    [10, '10 小时'],
    [10.49, '10 小时'],
    [10.5, '11 小时'],
  ]) {
    assert.equal(formatHours(value), expected);
    assert.equal(formatHours(value, 'hours'), expected);
  }
});

test('Issue 响应时长不足一小时按分钟显示，至少一分钟', () => {
  for (const [value, expected] of [
    [null, '—'],
    [0, '1 分钟'],
    [0.001, '1 分钟'],
    [0.024, '1 分钟'],
    [0.025, '2 分钟'],
    [0.5, '30 分钟'],
    [0.999, '60 分钟'],
    [1, '1.0 小时'],
    [9.96, '10.0 小时'],
    [10.5, '11 小时'],
  ]) {
    assert.equal(formatHours(value, 'minutes'), expected);
  }
});

test('两种时长格式保留负数和非有限值的既有显示', () => {
  assert.equal(formatHours(-0.5), '-0.5 小时');
  assert.equal(formatHours(-0.5, 'minutes'), '1 分钟');
  assert.equal(formatHours(Number.NaN), 'NaN 小时');
  assert.equal(formatHours(Number.NaN, 'minutes'), 'NaN 小时');
  assert.equal(formatHours(Number.POSITIVE_INFINITY), 'Infinity 小时');
  assert.equal(formatHours(Number.POSITIVE_INFINITY, 'minutes'), 'Infinity 小时');
  assert.equal(formatHours(Number.NEGATIVE_INFINITY), '-Infinity 小时');
  assert.equal(formatHours(Number.NEGATIVE_INFINITY, 'minutes'), '1 分钟');
});
