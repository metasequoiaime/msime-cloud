import assert from 'node:assert/strict';
import { test } from 'node:test';
import { MutationObserver, QueryClient, QueryObserver } from '@tanstack/react-query';
import { invalidateQueries } from '../src/api/cache.ts';
import { keys } from '../src/api/keys.ts';

function createClient(t) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity, staleTime: Infinity } } });
  t.after(() => client.clear());
  return client;
}

function observe(t, client, queryKey, queryFn) {
  client.setQueryData(queryKey, '旧数据');
  const observer = new QueryObserver(client, { queryKey, queryFn });
  t.after(observer.subscribe(() => {}));
  return observer;
}

test('仅使指定前缀的列表和详情失效，保留其他页面和身份缓存', async t => {
  const client = createClient(t);
  const selected = [keys.page('community', 'list', 2), keys.page('community', 'detail', 'skin-1'), keys.shell];
  const untouched = [keys.page('users'), keys.notifications, keys.session, ['other', 'community']];
  for (const key of [...selected, ...untouched]) client.setQueryData(key, '旧数据');

  const result = await invalidateQueries(client, keys.page('community'), keys.shell);
  for (const key of selected) assert.equal(client.getQueryState(key).isInvalidated, true);
  for (const key of untouched) assert.equal(client.getQueryState(key).isInvalidated, false);
  assert.deepEqual(result, [undefined, undefined]);
});

test('并行启动活跃查询，mutation 等待所有刷新完成才结束', async t => {
  const client = createClient(t);
  const users = Promise.withResolvers();
  const shell = Promise.withResolvers();
  const started = [];
  const usersKey = keys.page('users', 'list');
  const usersObserver = observe(t, client, usersKey, () => { started.push('users'); return users.promise; });
  observe(t, client, keys.shell, () => { started.push('shell'); return shell.promise; });
  const refreshing = Promise.withResolvers();
  const mutation = new MutationObserver(client, {
    mutationFn: async () => '已保存',
    onSettled: () => {
      const result = invalidateQueries(client, keys.page('users'), keys.shell);
      refreshing.resolve();
      return result;
    },
  });
  const finished = mutation.mutate();
  await refreshing.promise;
  assert.deepEqual(started, ['users', 'shell']);
  assert.equal(mutation.getCurrentResult().isPending, true);

  const usersUpdated = Promise.withResolvers();
  t.after(usersObserver.subscribe(result => { if (result.data === '新用户') usersUpdated.resolve(); }));
  users.resolve('新用户');
  await usersUpdated.promise;
  assert.equal(mutation.getCurrentResult().isPending, true);
  assert.equal(client.getQueryData(keys.shell), '旧数据');

  shell.resolve('新外壳');
  assert.equal(await finished, '已保存');
  assert.equal(mutation.getCurrentResult().isSuccess, true);
  assert.equal(client.getQueryData(keys.shell), '新外壳');
});

test('刷新失败沿用默认行为，保留旧数据并记录查询错误', async t => {
  const client = createClient(t);
  const failure = new Error('合成刷新失败');
  const observer = observe(t, client, keys.notifications, async () => { throw failure; });
  const result = await invalidateQueries(client, keys.notifications);
  assert.equal(observer.getCurrentResult().error, failure);
  assert.equal(client.getQueryData(keys.notifications), '旧数据');
  assert.deepEqual(result, [undefined]);
});

test('空范围不会使任何缓存失效', async t => {
  const client = createClient(t);
  client.setQueryData(keys.shell, '旧数据');
  assert.deepEqual(await invalidateQueries(client), []);
  assert.equal(client.getQueryState(keys.shell).isInvalidated, false);
});
