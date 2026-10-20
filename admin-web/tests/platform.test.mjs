import assert from 'node:assert/strict';
import { test } from 'node:test';
import { platformName } from '../src/utils/platform.ts';

test('平台名称共用大小写不敏感的别名与未知值回退', () => {
  assert.equal(platformName('WINDOWS'), 'Windows');
  assert.equal(platformName('harmonyos'), 'HarmonyOS');
  assert.equal(platformName('OHOS'), 'HarmonyOS');
  assert.equal(platformName('custom-platform'), 'custom-platform');
});
