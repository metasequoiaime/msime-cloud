import assert from 'node:assert/strict';
import { test } from 'node:test';
import { parsePage } from '../src/utils/pagination.ts';

test('页码缺失、空白或无数字时回到第一页', () => {
  for (const value of [undefined, '', '   ', 'abc', 'NaN', 'Infinity']) {
    assert.equal(parsePage(value), 1, String(value));
  }
});

test('页码保留十进制整数前缀、符号与截断规则', () => {
  for (const [value, expected] of [['1', 1], ['42', 42], ['  +0023  ', 23], ['2.9', 2], ['3suffix', 3], ['1e3', 1], ['0x10', 1]]) {
    assert.equal(parsePage(value), expected, value);
  }
});

test('页码始终限制在 1 至 10000，超大输入仍按边界处理', () => {
  for (const [value, expected] of [['-2', 1], ['-0', 1], ['0', 1], ['9999', 9999], ['10000', 10000], ['10001', 10000], ['9'.repeat(400), 10000], ['-' + '9'.repeat(400), 1]]) {
    assert.equal(parsePage(value), expected, value);
  }
});
