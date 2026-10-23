import assert from 'node:assert/strict';
import { test } from 'node:test';
import { lastPathSegment } from '../src/utils/path.ts';

test('最后路径段保留斜杠分隔字符串的原有边界语义', () => {
  for (const [value, expected] of [
    ['custom/words.txt', 'words.txt'],
    ['owner/repository', 'repository'],
    ['single', 'single'],
    ['', ''],
    ['/leading', 'leading'],
    ['trailing/', ''],
    ['nested/path/with spaces', 'with spaces'],
  ]) {
    assert.equal(lastPathSegment(value), expected, value);
  }
});
