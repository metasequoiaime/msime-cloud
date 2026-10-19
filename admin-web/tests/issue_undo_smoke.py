#!/usr/bin/env python3
"""验证 Issue 撤销采用服务端版本，冲突刷新，旧响应隐藏按钮。"""

import threading

from playwright.sync_api import expect, sync_playwright

import csp_smoke as smoke


def check_issue_undo(browser, base):
    ref = {"repo": smoke.SMOKE_ISSUE["repo"], "n": smoke.SMOKE_ISSUE["number"]}
    versioned = {**ref, "version": "a" * 64}
    scenarios = [
        ("triage", "确认并分类", {"action": "untriage", "items": [{**versioned, "keep_assignee": True}]}, "success"),
        ("mark_dup", "标记重复", {"action": "reopen", "items": [{**versioned, "keep_duplicate": True}]}, "success"),
        ("close", "关闭", {"action": "reopen", "items": [{**versioned, "keep_duplicate": True}]}, "success"),
        ("triage", "确认并分类", None, "hidden"),
        ("mark_dup", "标记重复", None, "hidden"),
        ("close", "关闭", None, "hidden"),
        ("triage", "确认并分类", "missing", "hidden"),
        ("triage", "确认并分类", {"action": "untriage", "items": [ref]}, "hidden"),
        ("triage", "确认并分类", {"action": "untriage", "items": [{**ref, "version": "bad"}]}, "hidden"),
        ("triage", "确认并分类", {"action": "untriage", "items": [{**versioned, "n": 999}]}, "hidden"),
        ("triage", "确认并分类", {"action": "reopen", "items": [versioned]}, "hidden"),
        ("close", "关闭", {"action": "reopen", "items": [versioned]}, "conflict"),
        ("triage", "确认并分类", {"action": "untriage", "items": [versioned]}, "partial_conflict"),
    ]
    for action, button, undo, outcome in scenarios:
        context = browser.new_context(viewport={"width": 1280, "height": 1000})
        page = context.new_page()
        requests, errors, reads = [], [], []
        page.on("pageerror", lambda error: errors.append(str(error)))
        page.add_init_script("window.cspViolations = []; document.addEventListener('securitypolicyviolation', event => window.cspViolations.push(event.violatedDirective));")
        page.route("**/api/auth/session", lambda route: route.fulfill(json=smoke.FIXTURES["/api/auth/session"]))
        page.route("**/api/shell", lambda route: route.fulfill(json={**smoke.FIXTURES["/api/shell"], "me": {**smoke.FIXTURES["/api/shell"]["me"], "permissions": ["triage_issues"]}}))

        def listing(route):
            reads.append(route.request.url)
            route.fulfill(json=smoke.FIXTURES["/api/issues"])

        page.route("**/api/issues?*", listing)

        def respond(route):
            requests.append(route.request.post_data_json)
            result = {"ok": True, "affected": 1, "failed": []}
            if undo != "missing":
                result["undo"] = undo if len(requests) == 1 else None
            if len(requests) > 1 and outcome == "conflict":
                route.fulfill(status=409, json={"error": {"code": "conflict"}})
                return
            if len(requests) > 1 and outcome == "partial_conflict":
                result["failed"] = [{**ref, "code": "conflict"}]
            route.fulfill(json=result)

        page.route("**/api/issues/actions", respond)
        try:
            page.goto(base + "/issues")
            if action == "close":
                page.get_by_role("checkbox", name="选择此行", exact=True).check()
            page.get_by_role("button", name=button, exact=True).click()
            if action == "close":
                page.get_by_role("dialog", name="关闭 #12？", exact=True).get_by_role("button", name="关闭", exact=True).click()
            expect(page.get_by_role("region", name="Notifications (F8)").get_by_text("#12 已分类" if action == "triage" else "#12 已关闭" if action == "close" else "#12 已标记为重复", exact=False)).to_be_visible()
            assert requests == [{"action": action, "items": [ref]}], requests
            if outcome != "hidden":
                before = len(reads)
                page.get_by_role("button", name="撤销", exact=True).click()
                message = "撤销失败：数据已被其他人修改，请刷新后重试。" if outcome == "conflict" else "撤销失败：已撤销 1 个，部分 Issue 已被修改，请核对最新状态" if outcome == "partial_conflict" else "已撤销"
                expect(page.get_by_role("region", name="Notifications (F8)").get_by_text(message, exact=True)).to_be_visible()
                assert requests[1] == undo, requests
                assert len(reads) > before, reads
            else:
                expect(page.get_by_role("button", name="撤销", exact=True)).not_to_be_visible()
            assert not errors, errors
            assert page.evaluate("window.cspViolations") == []
        finally:
            page.unroute_all(behavior="ignoreErrors")
            context.close()


if __name__ == "__main__":
    smoke.Handler.csp = smoke.production_csp()
    server = smoke.Server(("127.0.0.1", 0), smoke.Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    try:
        with sync_playwright() as playwright:
            browser = playwright.chromium.launch()
            try:
                check_issue_undo(browser, f"http://127.0.0.1:{server.server_port}")
                print("Issue 撤销浏览器回归通过（13 个场景）")
            finally:
                browser.close()
    finally:
        server.shutdown()
        server.server_close()
