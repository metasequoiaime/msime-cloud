#!/usr/bin/env python3
"""验证管理图片的 401 会清空登录状态，403 则保留当前身份。"""

import threading

from playwright.sync_api import expect, sync_playwright

import csp_smoke as smoke


def check_image_auth(browser, base):
    failures = []
    for mode in ("cookie", "token"):
        for status in (401, 403):
            context = browser.new_context()
            context.add_init_script("window.cspViolations = []; document.addEventListener('securitypolicyviolation', event => window.cspViolations.push(event.violatedDirective));")
            if mode == "cookie":
                context.add_cookies([{"name": "msime_admin_local", "value": "synthetic-session", "url": base}])
            page = context.new_page()
            errors = []
            page.on("pageerror", lambda error: errors.append(str(error)))
            session = {**smoke.FIXTURES["/api/auth/session"], "google_enabled": False, "token_enabled": True}
            feedback = smoke.FIXTURES["/api/feedback"]
            row = {**feedback["items"][0], "screenshots": 1}

            def session_response(route):
                authenticated = mode == "cookie" or route.request.headers.get("authorization") == "Bearer synthetic-admin-token"
                route.fulfill(json={**session, "authenticated": authenticated})

            page.route("**/api/auth/session", session_response)
            page.route("**/api/feedback?*", lambda route: route.fulfill(json={**feedback, "items": [row]}))
            page.route("**/api/feedback/*/screenshots/*", lambda route: route.fulfill(status=status, json={"error": {"code": "unauthorized" if status == 401 else "forbidden", "message": ""}}))
            try:
                with page.expect_response("**/api/feedback/*/screenshots/*") as image:
                    page.goto(base + f"/feedback?focus={row['id']}")
                    if mode == "token":
                        page.get_by_label("管理员密钥", exact=True).fill("synthetic-admin-token")
                        page.get_by_role("button", name="进入控制台", exact=True).click()
                assert image.value.status == status
                headers = image.value.request.all_headers()
                if mode == "cookie":
                    assert "msime_admin_local=synthetic-session" in headers.get("cookie", "")
                    assert "authorization" not in headers
                else:
                    assert headers.get("authorization") == "Bearer synthetic-admin-token"
                if status == 401:
                    expect(page.get_by_role("heading", name="水杉管理后台", exact=True)).to_be_visible()
                    expect(page.get_by_role("alert")).to_have_text("登录已失效，请重新登录。")
                    expect(page.get_by_role("complementary", name="后台导航")).to_have_count(0)
                    expect(page.get_by_text(row["text"], exact=True)).to_have_count(0)
                else:
                    expect(page.locator("header h1")).to_have_text("用户反馈")
                    expect(page.get_by_text("截图加载失败", exact=True)).to_be_visible()
                    expect(page.get_by_role("heading", name="水杉管理后台", exact=True)).to_have_count(0)
                assert not errors, errors
                assert page.evaluate("window.cspViolations") == []
            except AssertionError as error:
                failures.append(f"{mode} / {status}: {error}")
            finally:
                page.unroute_all(behavior="ignoreErrors")
                context.close()
    assert not failures, "\n".join(failures)


if __name__ == "__main__":
    smoke.Handler.csp = smoke.production_csp()
    server = smoke.Server(("127.0.0.1", 0), smoke.Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    try:
        with sync_playwright() as playwright:
            browser = playwright.chromium.launch()
            try:
                check_image_auth(browser, f"http://127.0.0.1:{server.server_port}")
            finally:
                browser.close()
        print("管理图片登录状态验证通过")
    finally:
        server.shutdown()
