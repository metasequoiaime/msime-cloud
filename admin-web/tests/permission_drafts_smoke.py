#!/usr/bin/env python3
"""验证权限列表刷新失败时保留管理员表单，以及首次读取失败后的恢复。"""

import threading

from playwright.sync_api import expect, sync_playwright

import csp_smoke as smoke


READ_ERROR = {"error": {"code": "unavailable", "message": "权限列表暂时不可用"}}


def check_permission_drafts(browser, base):
    for outcome in ("failure", "success"):
        context = browser.new_context(viewport={"width": 1280, "height": 1000})
        context.add_init_script("window.cspViolations = []; document.addEventListener('securitypolicyviolation', event => window.cspViolations.push(event.violatedDirective));")
        page = context.new_page()
        errors, posts, pending = [], [], []
        page.on("pageerror", lambda error: errors.append(str(error)))
        fail_reads = False

        def permissions(route):
            if fail_reads:
                route.fulfill(status=503, json=READ_ERROR)
            else:
                route.fulfill(json=smoke.FIXTURES["/api/permissions"])

        def admins(route):
            posts.append(route.request.post_data_json)
            pending.append(route)

        page.route("**/api/auth/session", lambda route: route.fulfill(json=smoke.FIXTURES["/api/auth/session"]))
        page.route("**/api/permissions", permissions)
        page.route("**/api/admins", admins)
        try:
            page.goto(base + "/perm")
            email = page.get_by_role("textbox", name="Google 账号邮箱", exact=True)
            role = page.get_by_role("combobox", name="角色", exact=True)
            add = page.get_by_role("button", name="添加管理员", exact=True)
            email.fill("first@example.com")
            role.select_option("operator")
            with page.expect_request("**/api/admins"):
                add.click()
            expect(add).to_be_disabled()
            if outcome == "success":
                # 已发出的请求继续使用原值，等待期间的新输入应保留为下一份草稿。
                email.fill("next@example.com")
                role.select_option("readonly")
            expected_email = "next@example.com" if outcome == "success" else "first@example.com"
            expected_role = "readonly" if outcome == "success" else "operator"
            fail_reads = True
            if outcome == "failure":
                pending.pop().fulfill(status=503, json={"error": {"code": "unavailable", "message": "添加暂时失败"}})
            else:
                pending.pop().fulfill(json={"ok": True, "affected": 1})
            alerts = page.locator("main").get_by_role("alert")
            expect(alerts.first).to_be_visible()
            expect(email).to_have_value(expected_email)
            expect(role).to_have_value(expected_role)
            expect(page.get_by_role("table", name="角色权限矩阵")).to_be_visible()
            expect(add).to_be_enabled()
            assert posts == [{"action": "add", "email": "first@example.com", "role": "operator"}], posts
            fail_reads = False
            alerts.first.get_by_role("button", name="重试", exact=True).click()
            expect(alerts).to_have_count(0)
            expect(email).to_have_value(expected_email)
            expect(role).to_have_value(expected_role)
            with page.expect_request("**/api/admins"):
                add.click()
            assert posts[-1] == {"action": "add", "email": expected_email, "role": expected_role}, posts
            pending.pop().fulfill(json={"ok": True, "affected": 1})
            expect(email).to_have_value("")
            expect(add).to_be_enabled()
            assert not errors, errors
            assert page.evaluate("window.cspViolations") == []
        finally:
            for route in pending:
                route.abort()
            page.unroute_all(behavior="ignoreErrors")
            context.close()


def check_permission_initial_failure(browser, base):
    context = browser.new_context(viewport={"width": 1280, "height": 1000})
    page = context.new_page()
    fail_reads = True

    def permissions(route):
        if fail_reads:
            route.fulfill(status=503, json=READ_ERROR)
        else:
            route.fulfill(json=smoke.FIXTURES["/api/permissions"])

    page.route("**/api/auth/session", lambda route: route.fulfill(json=smoke.FIXTURES["/api/auth/session"]))
    page.route("**/api/permissions", permissions)
    try:
        page.goto(base + "/perm")
        alerts = page.locator("main").get_by_role("alert")
        expect(alerts.first).to_be_visible()
        expect(page.get_by_role("textbox", name="Google 账号邮箱", exact=True)).to_have_count(0)
        expect(page.get_by_role("table", name="角色权限矩阵")).to_have_count(0)
        fail_reads = False
        alerts.first.get_by_role("button", name="重试", exact=True).click()
        expect(alerts).to_have_count(0)
        expect(page.get_by_role("textbox", name="Google 账号邮箱", exact=True)).to_have_value("")
        expect(page.get_by_role("combobox", name="角色", exact=True)).to_have_value("reviewer")
        expect(page.get_by_role("table", name="角色权限矩阵")).to_be_visible()
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
                base = f"http://127.0.0.1:{server.server_port}"
                check_permission_drafts(browser, base)
                check_permission_initial_failure(browser, base)
                print("管理员草稿保留与权限列表读取恢复回归通过")
            finally:
                browser.close()
    finally:
        server.shutdown()
        server.server_close()
