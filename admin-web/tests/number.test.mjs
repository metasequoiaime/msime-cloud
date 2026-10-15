import assert from 'node:assert/strict';
import { test } from 'node:test';
import { formatNumber } from '../src/utils/number.ts';

test('数字工具保留中英文千分位格式', () => {
  assert.equal(formatNumber(1234567.89), '1,234,567.89');
  assert.equal(formatNumber(1234567.89, 'zh-CN'), '1,234,567.89');
  assert.equal(formatNumber(1234567.89, 'en-US'), '1,234,567.89');
});

test('数字工具把 Intl 选项传给页面需要的小数位策略', () => {
  assert.equal(formatNumber(1234.567, 'zh-CN', { maximumFractionDigits: 0 }), '1,235');
  assert.equal(formatNumber(1234.5, 'zh-CN', { maximumFractionDigits: 2 }), '1,234.5');
});

test('数字工具保留默认精度和非有限值的既有显示', () => {
  assert.equal(formatNumber(-1234.5678), '-1,234.568');
  assert.equal(formatNumber(0), '0');
  assert.equal(formatNumber(Number.NaN), 'NaN');
  assert.equal(formatNumber(Number.POSITIVE_INFINITY), '∞');
});
