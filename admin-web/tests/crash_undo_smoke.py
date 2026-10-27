#!/usr/bin/env python3
"""验证崩溃分组撤销使用服务端版本，并在冲突后刷新列表。"""

import re
import threading

from playwright.sync_api import expect, sync_playwright

import csp_smoke as smoke


def check_crash_undo(browser, base):
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
        group = {**smoke.CRASH_GROUP, "status": "open", "issue_url": None}
        # 崩溃页默认带 ?version=latest 请求，路由要连查询参数一起匹配。
        page.route(re.compile(r".*/api/crash-groups(\?.*)?$"), lambda route: route.fulfill(json={**smoke.FIXTURES["/api/crash-groups"], "items": [group]}))
        page.route("**/api/crash-groups/0123456789abcdef", lambda route: route.fulfill(json={"group": group, "samples": []}))
        undo = {"action": "crash_group_status", "id": group["signature"], "value": {"status": "fixed", "token": "a" * 32}}

        def respond(route):
            body = route.request.post_data_json
            posts.append(body)
            if len(posts) == 1:
                assert body == {"action": "crash_group_status", "id": group["signature"], "value": "known"}, body
                group["status"] = "known"
                result = {"ok": True, "affected": 1, "previous": "fixed"}
                if scenario != "missing":
                    result["undo"] = undo if scenario != "invalid" else {**undo, "id": "another-group"}
                route.fulfill(json=result)
            else:
                group["status"] = "fixed"
                route.fulfill(status=409 if scenario == "conflict" else 200, json={"error": {"code": "conflict"}} if scenario == "conflict" else {"ok": True, "affected": 1})

        page.route("**/api/actions", respond)
        try:
            page.goto(base + "/crash?focus=" + group["signature"])
            page.get_by_role("dialog").get_by_role("button", name="标记为已知", exact=True).click()
            expect(page.get_by_text("已标记为已知问题", exact=True)).to_be_visible()
            button = page.get_by_role("button", name="撤销", exact=True, include_hidden=True)
            if scenario in ("missing", "invalid"):
                expect(button).to_have_count(0)
                assert len(posts) == 1, posts
            else:
                button.click()
                expect(page.get_by_text("撤销失败：数据已被其他人修改，请刷新后重试。" if scenario == "conflict" else "已撤销", exact=True)).to_be_visible()
                assert posts[1] == undo, posts
                expect(page.get_by_role("row").filter(has_text=group["title"]).get_by_text("已修复", exact=True)).to_be_visible()
            assert not errors, errors
            assert page.evaluate("window.cspViolations") == [], page.evaluate("window.cspViolations")
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
                check_crash_undo(browser, f"http://127.0.0.1:{server.server_port}")
                print("崩溃分组撤销浏览器验证通过（4 个场景）")
            finally:
                browser.close()
    finally:
        server.shutdown()
        server.server_close()
