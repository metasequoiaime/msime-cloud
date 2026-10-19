#!/usr/bin/env python3
"""验证封禁与解封的撤销使用服务端实际原因和版本。"""

import threading

from playwright.sync_api import expect, sync_playwright

import csp_smoke as smoke


def check_ban_undo(browser, base):
    for action in ("ban_user", "unban_user"):
        for scenario in ("success", "conflict", "missing", "invalid"):
            context = browser.new_context(viewport={"width": 1280, "height": 1000})
            page = context.new_page()
            page.clock.install()
            errors, posts = [], []
            page.on("pageerror", lambda error: errors.append(str(error)))
            page.add_init_script("window.cspViolations = []; document.addEventListener('securitypolicyviolation', e => window.cspViolations.push(e.violatedDirective));")
            shell = smoke.FIXTURES["/api/shell"]
            page.route("**/api/auth/session", lambda route: route.fulfill(json=smoke.FIXTURES["/api/auth/session"]))
            page.route("**/api/shell", lambda route: route.fulfill(json={**shell, "me": {**shell["me"], "permissions": ["ban_users"]}}))
            banned = action == "unban_user"
            row = {**smoke.FIXTURES["/api/users"]["items"][0], "banned": banned, "ban_reason": "旧页面原因" if banned else ""}
            detail = {**smoke.FIXTURES["/api/users/u1"], "banned": banned, "ban_reason": row["ban_reason"]}
            page.route("**/api/users?*", lambda route: route.fulfill(json={**smoke.FIXTURES["/api/users"], "items": [row]}))
            page.route("**/api/users/u1", lambda route: route.fulfill(json=detail))
            undo = {"action": "undo_user_ban", "id": "u1", "value": {"token": "a" * 32, "banned": banned}}
            if action == "unban_user":
                undo["reason"] = "服务端实际封禁原因"

            def respond(route):
                body = route.request.post_data_json
                posts.append(body)
                if len(posts) == 1:
                    assert body["action"] == action, body
                    row["banned"] = detail["banned"] = not banned
                    result = {"ok": True, "affected": 1, "sessions": 0, "removed": 0, "restored": 0}
                    if scenario != "missing":
                        result["undo"] = undo if scenario != "invalid" else {**undo, "id": "another-user"}
                    route.fulfill(json=result)
                else:
                    row["banned"] = detail["banned"] = banned if scenario == "success" else True
                    route.fulfill(status=409 if scenario == "conflict" else 200, json={"error": {"code": "conflict"}} if scenario == "conflict" else {"ok": True, "affected": 1, "sessions": 0, "removed": 0, "restored": 0})

            page.route("**/api/actions", respond)
            try:
                page.goto(base + "/users?focus=u1")
                page.get_by_role("dialog").get_by_role("button", name="封禁" if action == "ban_user" else "解除封禁", exact=True).click()
                if action == "ban_user":
                    page.get_by_role("dialog", name="封禁 smoke-user？", exact=True).get_by_role("button", name="封禁", exact=True).click()
                expect(page.get_by_text("smoke-user 已解除封禁" if action == "unban_user" else "smoke-user 已封禁：批量发布侵权内容", exact=True)).to_be_visible()
                button = page.get_by_role("button", name="撤销", exact=True, include_hidden=True)
                if scenario in ("missing", "invalid"):
                    expect(button).to_have_count(0)
                    assert len(posts) == 1, posts
                else:
                    button.click()
                    expect(page.get_by_text("撤销失败：数据已被其他人修改，请刷新后重试。" if scenario == "conflict" else "已撤销", exact=True)).to_be_visible()
                    assert posts[1] == undo, posts
                    expected_banned = banned if scenario == "success" else True
                    expect(page.get_by_role("row").filter(has_text="smoke-user").get_by_role("button", name="解除封禁" if expected_banned else "封禁", exact=True)).to_be_visible()
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
                check_ban_undo(browser, f"http://127.0.0.1:{server.server_port}")
                print("账号封禁撤销浏览器验证通过（8 个场景）")
            finally:
                browser.close()
    finally:
        server.shutdown()
        server.server_close()
