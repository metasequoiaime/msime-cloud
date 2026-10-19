#!/usr/bin/env python3
"""验证发布页连续搜索、历史导航及刷新后的展开状态。"""

import copy
import threading

from playwright.sync_api import expect, sync_playwright

import csp_smoke as smoke


def check_release_focus(browser, base):
    for initial in ("collapsed", "current"):
        context = browser.new_context(viewport={"width": 1280, "height": 1000})
        page = context.new_page()
        errors = []
        page.on("pageerror", lambda error: errors.append(str(error)))
        page.add_init_script("window.cspViolations = []; document.addEventListener('securitypolicyviolation', event => window.cspViolations.push(event.violatedDirective));")
        history = copy.deepcopy(smoke.FIXTURES["/api/releases/windows"])
        current, older = history["releases"]

        def search(route):
            route.fulfill(json={"items": [{"kind": "release", "id": f"windows:{release['tag']}", "title": release["version"], "where": "Windows 发布", "target": "release"} for release in history["releases"]]})

        def trigger(route):
            current["downloads"] += 1
            route.fulfill(json={"ok": True})

        page.route("**/api/auth/session", lambda route: route.fulfill(json=smoke.FIXTURES["/api/auth/session"]))
        page.route("**/api/shell", lambda route: route.fulfill(json={**smoke.FIXTURES["/api/shell"], "me": {**smoke.FIXTURES["/api/shell"]["me"], "permissions": ["trigger_release"]}}))
        page.route("**/api/search?*", search)
        page.route("**/api/releases/windows", lambda route: route.fulfill(json=history))
        page.route("**/api/releases/windows/trigger", trigger)
        current_row = page.locator(f'button[aria-controls="release-{current["id"]}"]')
        older_row = page.locator(f'button[aria-controls="release-{older["id"]}"]')

        def find_release(release, row):
            page.get_by_role("combobox", name="全局搜索").fill(release["version"])
            page.get_by_role("option", name=release["version"] + " Windows 发布", exact=True).click()
            expect(page).to_have_url(base + "/release?focus=windows%3A" + release["tag"])
            expect(row).to_have_attribute("aria-expanded", "true")

        try:
            page.goto(base + "/release?platform=windows")
            expect(current_row).to_have_attribute("aria-expanded", "true")
            current_row.click()
            if initial == "current":
                current_row.click()
            # 同平台组件已经有手动开合状态，再从真实搜索入口打开另一个版本。
            find_release(older, older_row)
            expect(current_row).to_have_attribute("aria-expanded", "false")
            expect(page.get_by_text(older["notes"][0]["text"], exact=True)).to_be_visible()
            older_row.click()
            find_release(current, current_row)
            expect(older_row).to_have_attribute("aria-expanded", "false")
            page.go_back()
            expect(older_row).to_have_attribute("aria-expanded", "true")
            page.go_forward()
            expect(current_row).to_have_attribute("aria-expanded", "true")

            # 搜索完成后仍可手动展开其他版本或全部折叠；普通数据刷新保留这些选择。
            older_row.click()
            for expanded, downloads in (("true", "128"), ("false", "129")):
                page.get_by_role("button", name="触发发布", exact=True).click()
                page.get_by_role("textbox", name="版本号", exact=True).fill("v0.5.5")
                with page.expect_response("**/api/releases/windows"):
                    page.get_by_role("dialog").get_by_role("button", name="触发发布", exact=True).click()
                expect(page.get_by_text(downloads, exact=True)).to_be_visible()
                expect(current_row).to_have_attribute("aria-expanded", "false")
                expect(older_row).to_have_attribute("aria-expanded", expanded)
                if expanded == "true":
                    older_row.click()
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
                check_release_focus(browser, f"http://127.0.0.1:{server.server_port}")
                print("发布页搜索跳转与刷新回归通过")
            finally:
                browser.close()
    finally:
        server.shutdown()
        server.server_close()
