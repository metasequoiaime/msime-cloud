#!/usr/bin/env python3
"""验证 Issue 回复跨页面保留等待状态，按仓库和编号隔离，并保留新草稿。"""

import re
import threading

from playwright.sync_api import expect, sync_playwright

import csp_smoke as smoke


def check_issue_reply_pending(browser, base):
    first = smoke.SMOKE_ISSUE
    others = [
        {**first, "number": 13, "title": "同仓库另一条问题"},
        {**first, "repo": "metasequoiaime/other", "title": "另一仓库同编号问题"},
    ]
    for success in (False, True):
        context = browser.new_context(viewport={"width": 1280, "height": 1000})
        page = context.new_page()
        errors, posts, writes, reads = [], [], [], []
        hold_reads = False
        page.on("pageerror", lambda error: errors.append(str(error)))
        page.add_init_script("window.cspViolations = []; document.addEventListener('securitypolicyviolation', event => window.cspViolations.push(event.violatedDirective));")

        def listing(route):
            if hold_reads:
                reads.append(route)
            else:
                route.fulfill(json={**smoke.FIXTURES["/api/issues"], "items": [first, *others], "total": 3})

        def comment(route):
            posts.append(route.request.post_data_json)
            writes.append(route)

        def open_issue(issue):
            page.get_by_role("row").filter(has_text=issue["title"]).click()
            expect(page.get_by_role("dialog")).to_contain_text(issue["title"])

        def leave_and_return():
            page.get_by_role("button", name="关闭", exact=True).click()
            page.locator('nav a[href="/perm"]').click()
            expect(page.get_by_role("heading", name="权限日志", exact=True)).to_be_visible()
            page.locator('nav a[href="/issues"]').click()
            expect(page.get_by_role("heading", name="问题分诊", exact=True)).to_be_visible()
            open_issue(first)

        page.route("**/api/auth/session", lambda route: route.fulfill(json=smoke.FIXTURES["/api/auth/session"]))
        page.route("**/api/shell", lambda route: route.fulfill(json={**smoke.FIXTURES["/api/shell"], "me": {**smoke.FIXTURES["/api/shell"]["me"], "permissions": ["triage_issues"]}}))
        page.route("**/api/issues?*", listing)
        for issue in [first, *others]:
            page.route(f'**/api/issues/{issue["repo"]}/{issue["number"]}', lambda route, request, issue=issue: route.fulfill(json={**smoke.FIXTURES["/api/issues/metasequoiaime/msime/12"], "issue": {**issue, "body": "合成问题正文"}}))
        page.route("**/api/issues/actions", comment)
        field = page.get_by_role("textbox", name="回复提交者…", exact=True)
        try:
            page.goto(base + "/issues")
            open_issue(first)
            field.fill("需要补充日志")
            with page.expect_request("**/api/issues/actions"):
                page.get_by_role("button", name="发送", exact=True).click()
            expect(page.get_by_role("button", name="发送中…", exact=True)).to_be_disabled()
            leave_and_return()
            field.fill("需要补充日志")
            if page.get_by_role("button", name=re.compile(r"^发送(?:中…)?$")).is_enabled():
                with page.expect_request("**/api/issues/actions"):
                    page.get_by_role("button", name="发送", exact=True).click()
            assert len(posts) == 1, f"同一 Issue 出现重复请求：{posts}"
            expect(page.get_by_role("button", name="发送中…", exact=True)).to_be_disabled()

            # 不同仓库或编号可以独立发送，切回原 Issue 仍须等待。
            for issue in others:
                page.get_by_role("button", name="关闭", exact=True).click()
                open_issue(issue)
                field.fill("另一条回复")
                expect(page.get_by_role("button", name="发送", exact=True)).to_be_enabled()
                with page.expect_request("**/api/issues/actions"):
                    page.get_by_role("button", name="发送", exact=True).click()
                writes.pop().fulfill(json={"ok": True, "affected": 1, "failed": []})
                expect(field).to_have_value("")
                page.get_by_role("button", name="关闭", exact=True).click()
                open_issue(first)
                field.fill("需要补充日志")
                expect(page.get_by_role("button", name="发送中…", exact=True)).to_be_disabled()
            assert len(posts) == 3, posts

            # 成功和失败都必须等列表刷新结束；原请求不能清掉返回后新写的草稿。
            hold_reads = True
            with page.expect_request("**/api/issues?*"):
                writes.pop().fulfill(status=200 if success else 503, json={"ok": True, "affected": 1, "failed": []} if success else {"error": {"code": "unavailable", "message": "合成发送失败"}})
            expect(page.get_by_role("button", name="发送中…", exact=True)).to_be_disabled()
            hold_reads = False
            for route in reads:
                listing(route)
            reads.clear()
            expect(page.get_by_role("button", name="发送", exact=True)).to_be_enabled()
            expect(field).to_have_value("需要补充日志")
            with page.expect_request("**/api/issues/actions"):
                page.get_by_role("button", name="发送", exact=True).click()
            assert posts == [
                {"action": "comment", "items": [{"repo": issue["repo"], "n": issue["number"]}], "body": body}
                for issue, body in [(first, "需要补充日志"), *[(issue, "另一条回复") for issue in others], (first, "需要补充日志")]
            ], posts
            field.fill("等待时继续写的新草稿")
            writes.pop().fulfill(json={"ok": True, "affected": 1, "failed": []})
            expect(page.get_by_role("button", name="发送", exact=True)).to_be_enabled()
            expect(field).to_have_value("等待时继续写的新草稿")
            assert not errors, errors
            assert page.evaluate("window.cspViolations") == []
        finally:
            for route in writes + reads:
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
                check_issue_reply_pending(browser, f"http://127.0.0.1:{server.server_port}")
                print("Issue 回复跨页面等待、隔离与草稿回归通过")
            finally:
                browser.close()
    finally:
        server.shutdown()
        server.server_close()
