import assert from 'node:assert/strict';
import { test } from 'node:test';
import { communityPathForSearch, navGroups, navItems, pageForPath, pageForTarget } from '../src/nav.ts';

const pages = [
  ['skins', '键盘皮肤'], ['candidate-skins', '候选窗皮肤'], ['plugins', '插件'],
  ['dictionaries', '社区词库'], ['replies', '回复模板'], ['phrases', '短语包'],
];

test('六类社区内容拥有独立入口、标题和社区导航分组', () => {
  const group = navGroups.find(group => group.title === '社区');
  assert.ok(group);
  assert.deepEqual(group.items.map(item => [item.key, item.label]), pages);
  assert.equal(navItems.some(item => item.key === 'community'), false);
  for (const [section, label] of pages) {
    assert.equal(pageForPath(`/${section}`)?.label, label);
    assert.equal(pageForTarget(section), `/${section}`);
  }
});

test('旧搜索和通知的社区目标按内容前缀定位独立页面', () => {
  for (const [section] of pages) {
    assert.equal(pageForTarget('community', `${section}/synthetic-id`), `/${section}`);
    assert.equal(pageForTarget('/community', `${section}/synthetic-id`), `/${section}`);
  }
  assert.equal(pageForTarget('community', 'image-skin/one'), '/skins');
  assert.equal(pageForTarget('community'), '/skins');
  assert.equal(pageForTarget('users', 'plugins/id'), '/users');
  assert.equal(pageForTarget('unknown', 'plugins/id'), '/');
});


test('旧入口保留分类选择，只有带斜线的合法前缀覆盖 tab', () => {
  for (const [section] of pages) {
    assert.equal(communityPathForSearch({ tab: section }), `/${section}`);
    assert.equal(communityPathForSearch({ tab: 'skins', focus: `${section}/id` }), `/${section}`);
  }
  assert.equal(communityPathForSearch({ tab: 'replies', focus: 'plugins' }), '/replies');
  assert.equal(communityPathForSearch({ tab: 'phrases', focus: 'image-skin/one' }), '/phrases');
  assert.equal(communityPathForSearch({ tab: 'unknown' }), '/skins');
});
