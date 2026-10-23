#!/usr/bin/env python3
"""验证反馈撤销使用服务端前态和版本，旧响应不提供撤销。"""

import threading

from playwright.sync_api import expect, sync_playwright

import csp_smoke as smoke


def check_feedback_undo(browser, base):
    for initial in ("new", "resolved"):
        for scenario in ("success", "conflict", "missing", "invalid"):
            context = browser.new_context(viewport={"width": 1280, "height": 1000})
            page = context.new_page()
            page.clock.install()
            errors, posts = [], []
            page.on("pageerror", lambda error: errors.append(str(error)))
            page.add_init_script("window.cspViolations = []; document.addEventListener('securitypolicyviolation', e => window.cspViolations.push(e.violatedDirective));")
            shell = smoke.FIXTURES["/api/shell"]
            page.route("**/api/auth/session", lambda route: route.fulfill(json=smoke.FIXTURES["/api/auth/session"]))
            page.route("**/api/shell", lambda route: route.fulfill(json={**shell, "me": {**shell["me"], "permissions": ["triage_issues"]}}))
            fixture = smoke.FIXTURES["/api/feedback"]
            item = {**fixture["items"][0], "status": initial}
            page.route("**/api/feedback?*", lambda route: route.fulfill(json={**fixture, "items": [item]}))
            undo = {"action": "undo_feedback_status", "id": item["id"], "value": {"status": initial, "token": "a" * 32}}
            target = "resolved" if initial == "new" else "new"

            def respond(route):
                body = route.request.post_data_json
                posts.append(body)
                if len(posts) == 1:
                    assert body == {"action": "resolve_feedback" if initial == "new" else "reopen_feedback", "id": item["id"]}, body
                    item["status"] = target
                    result = {"ok": True, "affected": 1}
                    if scenario != "missing":
                        result["undo"] = undo if scenario != "invalid" else {**undo, "id": "another-feedback"}
                    route.fulfill(json=result)
                else:
                    item["status"] = initial if scenario == "success" else target
                    route.fulfill(status=409 if scenario == "conflict" else 200, json={"error": {"code": "conflict"}} if scenario == "conflict" else {"ok": True, "affected": 1})

            page.route("**/api/actions", respond)
            try:
                page.goto(base + "/feedback")
                page.get_by_role("row").filter(has_text="smoke-user").get_by_role("button", name="标记已处理" if initial == "new" else "重新打开", exact=True).click()
                expect(page.get_by_text("已标记为已处理" if initial == "new" else "已重新打开", exact=True)).to_be_visible()
                button = page.get_by_role("button", name="撤销", exact=True)
                if scenario in ("missing", "invalid"):
                    expect(button).to_have_count(0)
                    assert len(posts) == 1, posts
                else:
                    button.click()
                    expect(page.get_by_text("撤销失败：数据已被其他人修改，请刷新后重试。" if scenario == "conflict" else "已撤销", exact=True)).to_be_visible()
                    assert posts[1] == undo, posts
                    state = initial if scenario == "success" else target
                    expect(page.get_by_role("row").filter(has_text="smoke-user").get_by_role("button", name="标记已处理" if state == "new" else "重新打开", exact=True)).to_be_visible()
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
                check_feedback_undo(browser, f"http://127.0.0.1:{server.server_port}")
                print("反馈撤销浏览器验证通过（8 个场景）")
            finally:
                browser.close()
    finally:
        server.shutdown()
        server.server_close()
