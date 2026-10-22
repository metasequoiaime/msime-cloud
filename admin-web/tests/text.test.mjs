import assert from 'node:assert/strict';
import { test } from 'node:test';
import { firstLine } from '../src/utils/text.ts';

test('取第一行并去掉首尾空白', () => {
  for (const [input, expected] of [
    ['  第一行  \n第二行', '第一行'],
    ['第一行\n第二行\n第三行', '第一行'],
    ['\n第二行', ''],
    ['\r\n第二行', ''],
    ['  单行文本  ', '单行文本'],
    ['', ''],
  ]) {
    assert.equal(firstLine(input), expected);
  }
});
