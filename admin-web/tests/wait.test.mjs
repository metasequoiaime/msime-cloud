import assert from 'node:assert/strict';
import { getEventListeners } from 'node:events';
import { test } from 'node:test';
import { waitOrAbort } from '../src/utils/wait.ts';

test('重复完成重连等待不会累积取消监听器', async context => {
  context.mock.timers.enable({ apis: ['setTimeout'] });
  const controller = new AbortController();
  for (let i = 0; i < 100; i++) {
    const waiting = waitOrAbort(500, controller.signal);
    context.mock.timers.tick(500);
    await waiting;
    assert.equal(getEventListeners(controller.signal, 'abort').length, 0);
  }
});

test('等待期间取消会立即结束等待并移除监听器', async context => {
  context.mock.timers.enable({ apis: ['setTimeout'] });
  const controller = new AbortController();
  const waiting = waitOrAbort(30_000, controller.signal);
  controller.abort();
  await waiting;
  assert.equal(getEventListeners(controller.signal, 'abort').length, 0);
});

test('已经取消的信号不再等待计时器或注册监听器', async context => {
  context.mock.timers.enable({ apis: ['setTimeout'] });
  const controller = new AbortController();
  controller.abort();
  let completed = false;
  const waiting = waitOrAbort(30_000, controller.signal).then(() => { completed = true; });
  await Promise.resolve();
  assert.equal(completed, true);
  await waiting;
  assert.equal(getEventListeners(controller.signal, 'abort').length, 0);
});
