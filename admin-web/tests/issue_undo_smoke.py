#!/usr/bin/env python3
"""验证 Issue 撤销仅采用服务端快照，兼容缺少撤销信息的旧服务端。"""

import threading

from playwright.sync_api import expect, sync_playwright

import csp_smoke as smoke


def check_issue_undo(browser, base):
    ref = {"repo": smoke.SMOKE_ISSUE["repo"], "n": smoke.SMOKE_ISSUE["number"]}
    for action, button, undo in (
        ("triage", "确认并分类", {"action": "untriage", "items": [{**ref, "keep_assignee": True}]}),
        ("mark_dup", "标记重复", {"action": "reopen", "items": [{**ref, "keep_duplicate": True}]}),
        ("triage", "确认并分类", None),
        ("mark_dup", "标记重复", None),
        ("close", "关闭", {"action": "reopen", "items": [{**ref, "keep_duplicate": True}]}),
        ("close", "关闭", None),
        ("triage", "确认并分类", "missing"),
    ):
        context = browser.new_context(viewport={"width": 1280, "height": 1000})
        page = context.new_page()
        requests, errors = [], []
        page.on("pageerror", lambda error: errors.append(str(error)))
        page.add_init_script("window.cspViolations = []; document.addEventListener('securitypolicyviolation', event => window.cspViolations.push(event.violatedDirective));")
        page.route("**/api/auth/session", lambda route: route.fulfill(json=smoke.FIXTURES["/api/auth/session"]))
        page.route("**/api/shell", lambda route: route.fulfill(json={**smoke.FIXTURES["/api/shell"], "me": {**smoke.FIXTURES["/api/shell"]["me"], "permissions": ["triage_issues"]}}))

        def respond(route):
            requests.append(route.request.post_data_json)
            result = {"ok": True, "affected": 1, "failed": []}
            if undo != "missing":
                result["undo"] = undo if len(requests) == 1 else None
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
            if isinstance(undo, dict):
                page.get_by_role("button", name="撤销", exact=True).click()
                expect(page.get_by_role("region", name="Notifications (F8)").get_by_text("已撤销", exact=True)).to_be_visible()
                assert requests[1] == undo, requests
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
                print("Issue 撤销快照回归通过")
            finally:
                browser.close()
    finally:
        server.shutdown()
        server.server_close()
