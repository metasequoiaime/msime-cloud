#!/usr/bin/env python3
"""验证崩溃分组的版本下拉框：保持服务端从新到旧的顺序、选择后带上 version 参数，切换平台时清掉版本筛选。"""

import re
import threading
from urllib.parse import parse_qs, urlsplit

from playwright.sync_api import expect, sync_playwright

import csp_smoke as smoke

VERSIONS = [{"version": "1.0.10", "count": 2}, {"version": "1.0.9", "count": 1}, {"version": "0.9.0", "count": 1}]


def check_crash_version_filter(browser, base):
    context = browser.new_context(viewport={"width": 1280, "height": 1000})
    page = context.new_page()
    errors, queries = [], []
    page.on("pageerror", lambda error: errors.append(str(error)))
    page.add_init_script("window.cspViolations = []; document.addEventListener('securitypolicyviolation', e => window.cspViolations.push(e.violatedDirective));")
    page.route("**/api/auth/session", lambda route: route.fulfill(json=smoke.FIXTURES["/api/auth/session"]))
    page.route("**/api/shell", lambda route: route.fulfill(json=smoke.FIXTURES["/api/shell"]))
    crash = smoke.FIXTURES["/api/crash-groups"]

    def respond(route):
        query = {key: values[0] for key, values in parse_qs(urlsplit(route.request.url).query).items()}
        queries.append(query)
        items = [item for item in crash["items"] if item["platform"] == query.get("platform", item["platform"])]
        # 版本筛选只留下第一个分组，模拟服务端按版本缩小列表。
        if query.get("version"):
            items = items[:1]
        route.fulfill(json={**crash, "items": items, "crash_versions": VERSIONS})

    page.route(re.compile(r".*/api/crash-groups(\?.*)?$"), respond)
    try:
        page.goto(base + "/crash")
        select = page.get_by_role("combobox", name="版本", exact=True)
        expect(select).to_have_value("")
        options = select.locator("option")
        expect(options).to_have_text(["全部版本", "1.0.10（2 个分组）", "1.0.9（1 个分组）", "0.9.0（1 个分组）"])
        rows = page.get_by_role("table", name="崩溃分组", exact=True).locator("tbody tr")
        expect(rows).to_have_count(2)

        select.select_option("1.0.9")
        expect(page).to_have_url(re.compile(r"[?&]version=1\.0\.9(&|$)"))
        expect(rows).to_have_count(1)
        assert queries[-1] == {"version": "1.0.9"}, queries

        # 链接里的版本即使已不在列表中，也作为当前选项显示。
        page.goto(base + "/crash?version=0.1.0")
        expect(select).to_have_value("0.1.0")
        expect(options.nth(1)).to_have_text("0.1.0（0 个分组）")

        page.get_by_role("radio", name=re.compile("^Windows")).click()
        expect(page).to_have_url(re.compile(r"[?&]platform=windows(&|$)"))
        expect(page).not_to_have_url(re.compile(r"version="))
        expect(select).to_have_value("")
        assert queries[-1] == {"platform": "windows"}, queries
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
                check_crash_version_filter(browser, f"http://127.0.0.1:{server.server_port}")
                print("崩溃分组版本筛选浏览器验证通过")
            finally:
                browser.close()
    finally:
        server.shutdown()
        server.server_close()
