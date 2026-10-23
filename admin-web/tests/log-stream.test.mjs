import assert from 'node:assert/strict';
import { test } from 'node:test';
import { openLogStream, parseSSE } from '../src/api/logs.ts';

const events = [
  { event: 'pods', id: '', data: '{"pods":["synthetic-pod"]}' },
  { event: 'lines', id: '123', data: '{"lines":[]}' },
  { event: 'gap', id: '124', data: '{"from":"123","to":"124"}' },
];

function parseChunks(chunks) {
  const events = [];
  let rest = '';
  for (const chunk of chunks) {
    const parsed = parseSSE(rest + chunk);
    rest = parsed.rest;
    events.push(...parsed.events);
  }
  return { events, rest };
}

function streamText(ending) {
  return [
    ': 合成心跳', '',
    'event: pods', 'data: {"pods":["synthetic-pod"]}', '',
    'id: 123', 'event: lines', 'data: {"lines":[]}', '',
    'event: gap', 'id: 124', 'data: {"from":"123","to":"124"}', '', '',
  ].join(ending);
}

test('日志 SSE 的合法换行在任意分块位置都保留事件类型、游标与正文', () => {
  for (const ending of ['\n', '\r\n', '\r']) {
    const text = streamText(ending);
    assert.deepEqual(parseChunks([text]).events, events);
    for (let split = 0; split <= text.length; split++) {
      assert.deepEqual(parseChunks([text.slice(0, split), text.slice(split)]).events, events, `换行=${JSON.stringify(ending)}，分块位置=${split}`);
    }
    assert.deepEqual(parseChunks([...text]).events, events);
  }
});

test('日志 SSE 保留未结束帧，多行正文、注释及混合换行不产生额外事件', () => {
  const text = ': 心跳\r\nid: 5\revent: lines\ndata: 第一行\r\ndata:  第二行\n\r\n';
  const expected = [{ event: 'lines', id: '5', data: '第一行\n 第二行' }];
  assert.deepEqual(parseChunks([...text]).events, expected);
  assert.deepEqual(parseChunks(['event: lines\r', '\nid: 5\r', '\ndata: 尚未结束']).events, []);
  const partial = parseChunks(['event: lines\r', '\nid: 5\r', '\ndata: 尚未结束']);
  assert.deepEqual(parseSSE(partial.rest + '\r\n\r\n').events, [{ event: 'lines', id: '5', data: '尚未结束' }]);
});

test('日志流逐字节读取 CRLF 和 UTF-8，分块不会丢失事件', async context => {
  const text = 'id: 123\r\nevent: lines\r\ndata: {"message":"合成日志"}\r\n\r\n';
  const bytes = new TextEncoder().encode(text);
  context.mock.method(globalThis, 'fetch', async () => new Response(new ReadableStream({
    start(controller) {
      for (const byte of bytes) controller.enqueue(Uint8Array.of(byte));
      controller.close();
    },
  }), { headers: { 'Content-Type': 'text/event-stream' } }));
  const received = [];
  await openLogStream(new URLSearchParams(), { token: '', signal: new AbortController().signal, onEvent: event => received.push(event) });
  assert.deepEqual(received, [{ event: 'lines', id: '123', data: '{"message":"合成日志"}' }]);
});

test('日志事件处理失败时取消仍打开的流，取消失败也保留原始错误', async context => {
  for (const failCancel of [false, true]) {
    let canceled = false;
    context.mock.method(globalThis, 'fetch', async () => new Response(new ReadableStream({
      start(controller) {
        controller.enqueue(new TextEncoder().encode('event: lines\ndata: {invalid synthetic json}\n\n'));
      },
      cancel() {
        canceled = true;
        if (failCancel) throw new Error('合成流取消失败');
      },
    })));
    await assert.rejects(openLogStream(new URLSearchParams(), {
      token: '', signal: new AbortController().signal,
      onEvent(event) { JSON.parse(event.data); },
    }), SyntaxError);
    assert.equal(canceled, true, '事件处理失败后，上游连接仍未取消');
  }
});
