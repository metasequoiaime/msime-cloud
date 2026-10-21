import assert from 'node:assert/strict';
import { test } from 'node:test';
import { formatRate } from '../src/utils/number.ts';

test('比率格式化区分空值与零，默认保留两位小数', () => {
  assert.equal(formatRate(null), '—');
  assert.equal(formatRate(undefined), '—');
  assert.equal(formatRate(0), '0.00%');
  assert.equal(formatRate(1), '100.00%');
  assert.equal(formatRate(0.1234567), '12.35%');
  assert.equal(formatRate(0.999999), '100.00%');
});

test('比率格式化保留调用方指定的小数位数', () => {
  assert.equal(formatRate(0.1234567, 0), '12%');
  assert.equal(formatRate(0.1234567, 1), '12.3%');
  assert.equal(formatRate(0.1234567, 3), '12.346%');
});

test('比率格式化保持超界值、负值和非有限值的既有显示', () => {
  assert.equal(formatRate(1.234), '123.40%');
  assert.equal(formatRate(-0.001), '-0.10%');
  assert.equal(formatRate(-0.00001), '-0.00%');
  assert.equal(formatRate(Number.NaN), 'NaN%');
  assert.equal(formatRate(Number.POSITIVE_INFINITY), 'Infinity%');
  assert.equal(formatRate(Number.NEGATIVE_INFINITY), '-Infinity%');
});

test('非法精度沿用原生错误，空值仍直接返回无数据文案', () => {
  assert.throws(() => formatRate(0.5, -1), RangeError);
  assert.throws(() => formatRate(0.5, 101), RangeError);
  assert.equal(formatRate(null, -1), '—');
  assert.equal(formatRate(undefined, 101), '—');
});
