#!/usr/bin/env python3
"""验证共用防抖副作用的计时、暂停、恢复与卸载，以及词库搜索。"""

import datetime
import json
from pathlib import Path
import re
import subprocess
import tempfile
import threading
from urllib.parse import parse_qs, urlsplit

from playwright.sync_api import expect, sync_playwright

import csp_smoke as smoke
from clock_helpers import expect_clocked_visible


def check_debounced_effect(browser, base):
    context = browser.new_context()
    page = context.new_page()
    errors = []
    page.on("pageerror", lambda error: errors.append(str(error)))
    page.add_init_script("window.cspViolations = []; document.addEventListener('securitypolicyviolation', e => window.cspViolations.push(e.violatedDirective));")
    try:
        page.clock.install(time=datetime.datetime(2026, 1, 1, tzinfo=datetime.timezone.utc))
        page.goto(base + "/harness/?effect")
        events = page.get_by_test_id("debounced-events")
        expect(events).to_have_text("[]")
        page.clock.pause_at(datetime.datetime(2026, 1, 1, 0, 1, tzinfo=datetime.timezone.utc))
        typed = page.get_by_role("textbox", name="副作用输入", exact=True)
        enabled = page.get_by_role("checkbox", name="启用副作用", exact=True)
        expected = []

        def assert_events():
            expect(events).to_have_text(json.dumps(expected, ensure_ascii=False, separators=(",", ":")))

        page.clock.run_for(500)
        assert_events()
        enabled.check()
        page.clock.run_for(199)
        assert_events()
        page.clock.run_for(1)
        expected.append("初始值")
        assert_events()
        typed.fill("中间值")
        page.clock.run_for(199)
        assert_events()
        typed.fill("最终值")
        page.clock.run_for(199)
        assert_events()
        page.clock.run_for(1)
        expected.append("最终值")
        assert_events()

        typed.fill("调整延迟")
        page.clock.run_for(100)
        page.get_by_role("combobox", name="副作用延迟", exact=True).select_option("400")
        page.clock.run_for(399)
        assert_events()
        page.clock.run_for(1)
        expected.append("调整延迟")
        assert_events()

        # 暂停必须取消已排队的副作用，恢复后重新等待完整时长。
        typed.fill("暂停值")
        page.clock.run_for(100)
        enabled.uncheck()
        page.clock.run_for(1000)
        assert_events()
        enabled.check()
        page.clock.run_for(399)
        assert_events()
        page.clock.run_for(1)
        expected.append("暂停值")
        assert_events()

        # 卸载后父组件继续记录，能发现仍然触发的旧回调；重新挂载只触发一次。
        typed.fill("重新挂载")
        page.clock.run_for(399)
        page.get_by_role("button", name="切换副作用挂载", exact=True).click()
        page.clock.run_for(1000)
        assert_events()
        page.get_by_role("button", name="切换副作用挂载", exact=True).click()
        page.clock.run_for(399)
        assert_events()
        page.clock.run_for(1)
        expected.append("重新挂载")
        assert_events()
        assert not errors, errors
        assert page.evaluate("window.cspViolations") == []
    finally:
        context.close()


def check_dictionary_debounce(browser, base):
    context = browser.new_context(viewport={"width": 1280, "height": 1000})
    page = context.new_page()
    requests, errors = [], []
    page.on("pageerror", lambda error: errors.append(str(error)))
    page.add_init_script("window.cspViolations = []; document.addEventListener('securitypolicyviolation', e => window.cspViolations.push(e.violatedDirective));")

    def listing(route):
        query = urlsplit(route.request.url).query
        params = parse_qs(query)
        if params["path"] == ["packs/programming/quanpin.txt"]:
            requests.append((params["page"][0], params.get("q", [""])[0]))
        route.fulfill(json=smoke.dictionary_fixture(query))

    page.route("**/api/auth/session", lambda route: route.fulfill(json=smoke.FIXTURES["/api/auth/session"]))
    page.route("**/api/dictionary/file?*", listing)
    try:
        page.clock.install(time=datetime.datetime(2026, 1, 1, tzinfo=datetime.timezone.utc))
        page.goto(base + "/dictionary")
        page.get_by_role("radio", name=re.compile("专题词包")).click()
        expect(page.get_by_text("共 105 条词条", exact=False)).to_be_visible()
        page.clock.pause_at(datetime.datetime(2026, 1, 1, 0, 1, tzinfo=datetime.timezone.utc))
        with page.expect_response("**/api/dictionary/file?*"):
            page.get_by_role("button", name="下一页", exact=True).click()
        expect_clocked_visible(page, page.get_by_text("第 2 / 2 页", exact=False))
        typed = page.get_by_role("textbox", name="搜索当前文件词条", exact=True)
        initial = len(requests)
        typed.fill("中间词")
        page.clock.run_for(349)
        assert len(requests) == initial, requests
        typed.fill("  测试  ")
        page.clock.run_for(349)
        assert len(requests) == initial, requests
        with page.expect_response("**/api/dictionary/file?*"):
            page.clock.run_for(1)
        expect_clocked_visible(page, page.get_by_text("找到 104 条「测试」的匹配", exact=False))
        assert requests[initial:] == [("1", "测试")], requests

        # 手动搜索与清空立即生效，不能在旧计时器到期后再查询一次。
        typed.fill("示例")
        with page.expect_response("**/api/dictionary/file?*"):
            page.get_by_role("button", name="搜索词条", exact=True).click()
        expect_clocked_visible(page, page.get_by_text("找到 1 条「示例」的匹配", exact=False))
        count = len(requests)
        page.clock.run_for(1000)
        assert len(requests) == count, requests
        with page.expect_response("**/api/dictionary/file?*"):
            page.get_by_role("button", name="清空词条搜索", exact=True).click()
        expect_clocked_visible(page, page.get_by_text("共 105 条词条", exact=False))

        # 编辑期间取消待应用搜索，提交单元格后再等待完整的 350 毫秒。
        typed.fill("测试")
        page.clock.run_for(200)
        cell = page.get_by_role("grid", name="词条电子表格").locator('[data-cell="C1"]')
        cell.click()
        editor = cell.get_by_role("textbox")
        editor.fill("250")
        count = len(requests)
        page.clock.run_for(1000)
        assert len(requests) == count, requests
        expect(page.get_by_text("共 105 条词条", exact=False)).to_be_visible()
        expect(editor).to_have_value("250")
        editor.press("Enter")
        page.clock.run_for(349)
        assert len(requests) == count, requests
        expect(page.get_by_text("共 105 条词条", exact=False)).to_be_visible()
        # 之前搜索过的结果仍在 60 秒缓存内，因此恢复时验证页面而不要求重新发请求。
        page.clock.run_for(1)
        expect_clocked_visible(page, page.get_by_text("找到 104 条「测试」的匹配", exact=False))
        expect(page.get_by_text("待保存变更 · 1 项", exact=False)).to_be_visible()
        assert all(q != "中间词" for _, q in requests), requests
        assert not errors, errors
        assert page.evaluate("window.cspViolations") == []
    finally:
        page.unroute_all(behavior="ignoreErrors")
        context.close()


if __name__ == "__main__":
    smoke.Handler.csp = smoke.production_csp()
    with tempfile.TemporaryDirectory(prefix="msime-search-harness-") as directory:
        subprocess.run(["pnpm", "exec", "vite", "build", "--logLevel", "warn", "--config", "tests/harness/vite.config.ts", "--outDir", directory], cwd=smoke.ROOT / "admin-web", check=True)
        smoke.Handler.harness = Path(directory)
        server = smoke.Server(("127.0.0.1", 0), smoke.Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        try:
            with sync_playwright() as playwright:
                browser = playwright.chromium.launch()
                try:
                    base = f"http://127.0.0.1:{server.server_port}"
                    problems = []
                    smoke.check_debounce(browser, base, problems)
                    assert not problems, problems
                    check_debounced_effect(browser, base)
                    check_dictionary_debounce(browser, base)
                finally:
                    browser.close()
            print("搜索防抖副作用验证通过")
        finally:
            server.shutdown()
