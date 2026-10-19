#!/usr/bin/env python3
"""验证崩溃分组历史导航和词库 PR 历史目标定位。"""

import copy
import threading

from playwright.sync_api import expect, sync_playwright

import csp_smoke as smoke


def check_crash_focus_history(browser, base):
    context = browser.new_context(viewport={"width": 1280, "height": 1000})
    page = context.new_page()
    errors = []
    page.on("pageerror", lambda error: errors.append(str(error)))
    page.add_init_script("window.cspViolations = []; document.addEventListener('securitypolicyviolation', event => window.cspViolations.push(event.violatedDirective));")
    first, second = smoke.FIXTURES["/api/crash-groups"]["items"]
    page.route("**/api/auth/session", lambda route: route.fulfill(json=smoke.FIXTURES["/api/auth/session"]))
    page.route(f'**/api/crash-groups/{second["signature"]}', lambda route: route.fulfill(json={"group": second, "samples": []}))
    page.route("**/api/search?*", lambda route: route.fulfill(json={"items": [{"kind": "crash_group", "id": second["signature"], "title": second["title"], "where": "崩溃上报", "target": "crash"}]}))
    try:
        page.goto(base + "/crash")
        page.get_by_role("combobox", name="全局搜索").fill(second["title"])
        page.get_by_role("option", name=second["title"] + " 崩溃上报", exact=True).click()
        expect(page.get_by_role("dialog", name=second["title"], exact=True)).to_be_visible()
        page.go_back()
        expect(page.get_by_role("dialog")).not_to_be_visible()
        page.get_by_role("button", name="查看堆栈", exact=True).click()
        expect(page.get_by_role("dialog", name=first["title"], exact=True)).to_be_visible()
        # 手动打开分组后，浏览器前进必须恢复 URL 指定的另一个分组。
        page.go_forward()
        expect(page.get_by_role("dialog", name=second["title"], exact=True)).to_be_visible()
        page.go_back()
        expect(page.get_by_role("dialog", name=first["title"], exact=True)).to_be_visible()
        page.get_by_role("button", name="关闭", exact=True).click()
        expect(page.get_by_role("dialog")).not_to_be_visible()
        expect(page).to_have_url(base + "/crash")
        assert not errors, errors
        assert page.evaluate("window.cspViolations") == []
    finally:
        page.unroute_all(behavior="ignoreErrors")
        context.close()


def check_dictpr_focus_history(browser, base):
    for state in ("open", "merged", "closed"):
        context = browser.new_context(viewport={"width": 1280, "height": 1000})
        page = context.new_page()
        errors, reads = [], []
        page.on("pageerror", lambda error: errors.append(str(error)))
        page.add_init_script("window.cspViolations = []; document.addEventListener('securitypolicyviolation', event => window.cspViolations.push(event.violatedDirective));")
        target = copy.deepcopy(smoke.FIXTURES["/api/dict-prs/9"])
        target["pull"].update(number=7, title="历史目标词库投稿", state=state, url="https://github.com/metasequoiaime/msime-dictionary/pull/7")

        def detail(route):
            reads.append(route.request.url)
            route.fulfill(json=target if route.request.url.endswith("/7") else smoke.FIXTURES["/api/dict-prs/9"])

        page.route("**/api/auth/session", lambda route: route.fulfill(json=smoke.FIXTURES["/api/auth/session"]))
        page.route("**/api/dict-prs/*", detail)
        try:
            page.goto(base + "/dictpr?focus=7")
            expect(page.get_by_role("heading", name=target["pull"]["title"], exact=True)).to_be_visible()
            expect(page.get_by_role("list", name="词库 PR 列表").get_by_role("button").filter(has_text="#7")).to_have_attribute("aria-current", "true")
            expect(page.get_by_role("list", name="#7 的词条", exact=True)).to_be_visible()
            if state == "open":
                expect(page.get_by_role("button", name="审核通过并合并", exact=True)).to_be_enabled()
            else:
                expect(page.get_by_role("button", name="审核通过并合并", exact=True)).to_be_disabled()
            assert reads == [base + "/api/dict-prs/7"], reads
            assert not errors, errors
            assert page.evaluate("window.cspViolations") == []
        finally:
            page.unroute_all(behavior="ignoreErrors")
            context.close()


def check_dictpr_empty_filter(browser, base):
    context = browser.new_context(viewport={"width": 1280, "height": 1000})
    page = context.new_page()
    page.route("**/api/auth/session", lambda route: route.fulfill(json=smoke.FIXTURES["/api/auth/session"]))
    try:
        page.goto(base + "/dictpr?state=closed")
        expect(page.get_by_text("这一栏是空的", exact=True)).to_be_visible()
        expect(page.get_by_role("heading", name=smoke.FIXTURES["/api/dict-prs/9"]["pull"]["title"], exact=True)).not_to_be_visible()
        expect(page.get_by_role("button", name="审核通过并合并", exact=True)).not_to_be_visible()
        expect(page.get_by_text("当前状态下没有词库 PR", exact=True)).to_be_visible()
    finally:
        page.unroute_all(behavior="ignoreErrors")
        context.close()


def check_dictpr_focus_unavailable(browser, base):
    for outcome in ("retry", "missing", "invalid"):
        context = browser.new_context(viewport={"width": 1280, "height": 1000})
        page = context.new_page()
        pending, reads, errors = [], [], []
        page.on("pageerror", lambda error: errors.append(str(error)))
        page.add_init_script("window.cspViolations = []; document.addEventListener('securitypolicyviolation', event => window.cspViolations.push(event.violatedDirective));")

        def detail(route):
            reads.append(route.request.url)
            pending.append(route)

        page.route("**/api/auth/session", lambda route: route.fulfill(json=smoke.FIXTURES["/api/auth/session"]))
        page.route("**/api/dict-prs/*", detail)
        try:
            page.goto(base + "/dictpr?focus=" + ("abc" if outcome == "invalid" else "7"))
            expect(page.get_by_role("list", name="词库 PR 列表")).to_be_visible()
            expect(page.get_by_role("heading", name=smoke.FIXTURES["/api/dict-prs/9"]["pull"]["title"], exact=True)).not_to_be_visible()
            expect(page.get_by_role("button", name="审核通过并合并", exact=True)).not_to_be_visible()
            if outcome == "invalid":
                expect(page.get_by_text("定位的词库 PR 编号无效", exact=True)).to_be_visible()
                assert not reads, reads
                assert not errors, errors
                assert page.evaluate("window.cspViolations") == []
                continue
            expect(page.get_by_role("status", name="正在加载")).to_be_visible()
            assert reads == [base + "/api/dict-prs/7"], reads
            pending.pop().fulfill(status=500 if outcome == "retry" else 404, json={"error": {"code": "internal_error" if outcome == "retry" else "not_found", "message": "读取失败"}})
            expect(page.get_by_role("alert")).to_be_visible()
            expect(page.get_by_role("button", name="审核通过并合并", exact=True)).not_to_be_visible()
            if outcome == "retry":
                with page.expect_request("**/api/dict-prs/7"):
                    page.get_by_role("alert").get_by_role("button", name="重试", exact=True).click()
                expect(page.get_by_role("status", name="正在加载")).to_be_visible()
                target = copy.deepcopy(smoke.FIXTURES["/api/dict-prs/9"])
                target["pull"].update(number=7, title="重试成功的历史投稿", url="https://github.com/metasequoiaime/msime-dictionary/pull/7")
                pending.pop().fulfill(json=target)
                expect(page.get_by_role("heading", name=target["pull"]["title"], exact=True)).to_be_visible()
            assert not errors, errors
            assert page.evaluate("window.cspViolations") == []
        finally:
            for route in pending:
                route.abort()
            page.unroute_all(behavior="ignoreErrors")
            context.close()


if __name__ == "__main__":
    smoke.Handler.csp = smoke.production_csp()
    server = smoke.Server(("127.0.0.1", 0), smoke.Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    failures = []
    try:
        with sync_playwright() as playwright:
            browser = playwright.chromium.launch()
            try:
                for check in (check_crash_focus_history, check_dictpr_focus_history, check_dictpr_empty_filter, check_dictpr_focus_unavailable):
                    try:
                        check(browser, f"http://127.0.0.1:{server.server_port}")
                    except AssertionError as error:
                        failures.append(f"{check.__name__}: {error}")
                assert not failures, "\n".join(failures)
                print("崩溃分组历史导航和词库 PR 定位回归通过")
            finally:
                browser.close()
    finally:
        server.shutdown()
        server.server_close()
