#!/usr/bin/env python3
"""在后台 CSP 下验证日志流重连、暂停缓冲和筛选重置。"""

import threading

from playwright.sync_api import expect, sync_playwright

import csp_smoke as smoke


def check_log_stream_replay(browser, base):
    context = browser.new_context(viewport={"width": 1280, "height": 1000})
    page = context.new_page()
    errors = []
    page.on("pageerror", lambda error: errors.append(str(error)))
    shell = smoke.FIXTURES["/api/shell"]
    page.route("**/api/auth/session", lambda route: route.fulfill(json=smoke.FIXTURES["/api/auth/session"]))
    page.route("**/api/shell", lambda route: route.fulfill(json={**shell, "features": {"logs": True}, "me": {**shell["me"], "permissions": ["view_logs"]}}))
    page.add_init_script("""(() => {
      window.cspViolations = [];
      document.addEventListener('securitypolicyviolation', e => window.cspViolations.push(e.violatedDirective));
      const original = window.fetch.bind(window);
      window.logStreams = [];
      window.fetch = (input, options) => {
        if (!String(input).startsWith('/api/logs/stream?')) return original(input, options);
        return Promise.resolve(new Response(new ReadableStream({start(controller) {
          window.logStreams.push({controller, url: String(input)});
          options.signal.addEventListener('abort', () => controller.error(new DOMException('已取消', 'AbortError')), {once: true});
        }}), {headers: {'Content-Type': 'text/event-stream'}}));
      };
      window.sendLogLines = lines => window.logStreams.at(-1).controller.enqueue(new TextEncoder().encode(
        'id: 1790866021452052719\\nevent: lines\\ndata: ' + JSON.stringify({lines}) + '\\n\\n'));
    })();""")
    common = {"ts": "1790866021452052720", "time": "2026-10-01T14:47:01Z", "pod": "synthetic-pod", "stream": "stderr", "level": "INFO"}
    first = {**common, "id": "first", "message": "重连前的日志"}
    second = {**common, "id": "second", "message": "重连后的日志"}
    try:
        page.goto(base + "/logs")
        page.wait_for_function("() => window.logStreams.length === 1")
        log = page.get_by_role("log", name="服务日志")
        page.evaluate("lines => window.sendLogLines(lines)", [first])
        expect(log.get_by_text(first["message"], exact=True)).to_have_count(1)
        page.evaluate("window.logStreams.at(-1).controller.close()")
        page.wait_for_function("() => window.logStreams.length === 2")
        assert "cursor=1790866021452052719" in page.evaluate("window.logStreams.at(-1).url")
        page.evaluate("lines => window.sendLogLines(lines)", [first, second])
        expect(log.get_by_text(second["message"], exact=True)).to_have_count(1)
        expect(log.get_by_text(first["message"], exact=True)).to_have_count(1)

        page.get_by_role("button", name="暂停", exact=True).click()
        page.evaluate("window.logStreams.at(-1).controller.close()")
        page.wait_for_function("() => window.logStreams.length === 3")
        buffered = {**common, "id": "buffered", "message": "暂停期间的新日志"}
        # 两条原文不同的日志截断后可以完全相同，仍须按服务端标识分别保留。
        same_display = [{**common, "id": key, "message": "截断后相同的内容"} for key in ("long-a", "long-b")]
        page.evaluate("lines => window.sendLogLines(lines)", [first, second, buffered, buffered, *same_display])
        expect(page.get_by_role("status").filter(has_text="已暂停，缓冲 3 行")).to_be_visible()
        page.get_by_role("button", name="继续", exact=True).click()
        expect(log.get_by_text(buffered["message"], exact=True)).to_have_count(1)
        expect(log.get_by_text("截断后相同的内容", exact=True)).to_have_count(2)
        expect(log.get_by_text(first["message"], exact=True)).to_have_count(1)

        page.get_by_label("搜索日志内容").fill("新的筛选")
        page.wait_for_function("() => window.logStreams.length === 4")
        page.evaluate("lines => window.sendLogLines(lines)", [first])
        expect(log.get_by_text(first["message"], exact=True)).to_have_count(1)
        expect(log.get_by_text(second["message"], exact=True)).to_have_count(0)
        legacy = {**common, "message": "旧副本未提供标识"}
        page.evaluate("lines => window.sendLogLines(lines)", [legacy, legacy])
        expect(log.get_by_text(legacy["message"], exact=True)).to_have_count(2)

        # 网络中断后自动重连；连接持续打开时也要及时清除旧错误。
        stream_count = page.evaluate("window.logStreams.length")
        page.evaluate("window.logStreams.at(-1).controller.error(new TypeError('测试断线'))")
        expect(page.get_by_role("alert").filter(has_text="测试断线")).to_be_visible()
        page.wait_for_function("count => window.logStreams.length === count + 1", arg=stream_count)
        recovered = {**common, "id": "recovered", "message": "连接恢复后的日志"}
        page.evaluate("lines => window.sendLogLines(lines)", [recovered])
        expect(log.get_by_text(recovered["message"], exact=True)).to_have_count(1)
        expect(page.get_by_role("status").filter(has_text="实时")).to_be_visible()
        expect(page.get_by_role("alert")).to_have_count(0)
        expect(log.get_by_text(first["message"], exact=True)).to_have_count(1)
        assert not errors, errors
        assert not page.evaluate("window.cspViolations"), page.evaluate("window.cspViolations")
    finally:
        context.close()


if __name__ == "__main__":
    smoke.Handler.csp = smoke.production_csp()
    server = smoke.Server(("127.0.0.1", 0), smoke.Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    try:
        with sync_playwright() as playwright:
            browser = playwright.chromium.launch()
            try:
                check_log_stream_replay(browser, f"http://127.0.0.1:{server.server_address[1]}")
            finally:
                browser.close()
        print("日志流重连回归通过")
    finally:
        server.shutdown()
        server.server_close()
