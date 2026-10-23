#!/usr/bin/env python3
"""验证个人访问令牌生成跨页面保留等待状态，避免重复生成使新令牌立即失效。"""

import copy
import threading

from playwright.sync_api import expect, sync_playwright

import csp_smoke as smoke


def check_admin_token_pending(browser, base):
    for existing in (False, True):
        for success in (False, True):
            context = browser.new_context(viewport={"width": 1280, "height": 1000})
            context.add_init_script("window.cspViolations = []; document.addEventListener('securitypolicyviolation', event => window.cspViolations.push(event.violatedDirective));")
            page = context.new_page()
            errors, posts, pending_writes, pending_reads = [], [], [], []
            profile = copy.deepcopy(smoke.FIXTURES["/api/me"])
            if not existing:
                profile["token"] = None
            hold_reads = False
            page.on("pageerror", lambda error: errors.append(str(error)))

            def me(route):
                if route.request.method == "POST":
                    posts.append(route.request.post_data_json)
                    pending_writes.append(route)
                elif hold_reads:
                    pending_reads.append(route)
                else:
                    route.fulfill(json=profile)

            def generate():
                page.get_by_role("button", name="重新生成" if profile["token"] else "生成", exact=True).click()
                if profile["token"]:
                    page.get_by_role("dialog").get_by_role("button", name="重新生成", exact=True).click()

            def navigate_back():
                page.locator('nav a[href="/perm"]').click()
                expect(page.get_by_role("heading", name="权限日志", exact=True)).to_be_visible()
                page.locator('aside a[href="/me"]').click()
                expect(page.get_by_role("heading", name="个人中心", exact=True)).to_be_visible()

            page.route("**/api/auth/session", lambda route: route.fulfill(json=smoke.FIXTURES["/api/auth/session"]))
            page.route("**/api/me", me)
            try:
                page.goto(base + "/me")
                with page.expect_request("**/api/me"):
                    generate()
                button = page.get_by_role("button", name="重新生成" if existing else "生成", exact=True)
                expect(button).to_be_disabled()
                navigate_back()
                expect(button).to_be_disabled()
                assert posts == [{"action": "regenerate_token"}], posts
                if success:
                    # 写入完成后仍需等待当前页面资料刷新，不能立即重复生成。
                    hold_reads = True
                    profile["token"] = {**smoke.FIXTURES["/api/me"]["token"], "last4": "test"}
                    with page.expect_request("**/api/me"):
                        pending_writes.pop().fulfill(json={"ok": True, "token": "msime_pat_synthetic_first_test", **profile["token"]})
                    expect(button).to_be_disabled()
                    hold_reads = False
                    for route in pending_reads:
                        route.fulfill(json=profile)
                    pending_reads.clear()
                else:
                    pending_writes.pop().fulfill(status=503, json={"error": {"code": "unavailable", "message": "合成签发失败"}})
                    expect(page.get_by_text("操作失败：", exact=False).first).to_be_visible()
                button = page.get_by_role("button", name="重新生成" if profile["token"] else "生成", exact=True)
                expect(button).to_be_enabled()
                with page.expect_request("**/api/me"):
                    generate()
                assert posts == [{"action": "regenerate_token"}] * 2, posts
                profile["token"] = {**smoke.FIXTURES["/api/me"]["token"], "last4": "next"}
                pending_writes.pop().fulfill(json={"ok": True, "token": "msime_pat_synthetic_second_next", **profile["token"]})
                expect(page.get_by_text("msime_pat_synthetic_second_next", exact=True)).to_be_visible()
                expect(page.get_by_role("button", name="重新生成", exact=True)).to_be_enabled()
                assert not errors, errors
                assert page.evaluate("window.cspViolations") == []
            finally:
                for route in pending_writes + pending_reads:
                    route.abort()
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
                check_admin_token_pending(browser, f"http://127.0.0.1:{server.server_port}")
                print("个人访问令牌生成等待与页面切换回归通过")
            finally:
                browser.close()
    finally:
        server.shutdown()
        server.server_close()
