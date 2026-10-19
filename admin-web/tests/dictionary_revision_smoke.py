#!/usr/bin/env python3
"""验证词库清空草稿后的版本切换与未保存变更的冲突保护。"""

import re
import threading
from urllib.parse import urlsplit

from playwright.sync_api import expect, sync_playwright

import csp_smoke as smoke


def check_dictionary_revisions(browser, base):
    for clearing in ("undo", "row", "original", "dirty"):
        context = browser.new_context(viewport={"width": 1280, "height": 1000})
        context.add_init_script("window.cspViolations = []; document.addEventListener('securitypolicyviolation', event => window.cspViolations.push(event.violatedDirective));")
        page = context.new_page()
        errors = []
        page.on("pageerror", lambda error: errors.append(str(error)))
        revision = "a" * 40
        posts = []

        def file_response(route):
            fixture = smoke.dictionary_fixture(urlsplit(route.request.url).query)
            fixture["sha"] = revision
            route.fulfill(json=fixture)

        def save_response(route):
            posts.append(route.request.post_data_json)
            route.fulfill(json=smoke.FIXTURES["/api/dictionary/changes"])

        page.route("**/api/auth/session", lambda route: route.fulfill(json=smoke.FIXTURES["/api/auth/session"]))
        page.route("**/api/dictionary/file?*", file_response)
        page.route("**/api/dictionary/changes", save_response)
        try:
            page.goto(base + "/dictionary")
            page.get_by_role("radio", name=re.compile("专题词包")).click()
            table = page.get_by_role("grid", name="词条电子表格")

            def edit_weight(value):
                cell = table.locator('[data-cell="C1"]')
                cell.click()
                cell.get_by_role("textbox").fill(value)
                cell.get_by_role("textbox").press("Enter")

            edit_weight("250")
            expect(page.get_by_text("待保存变更 · 1 项", exact=False)).to_be_visible()
            if clearing == "undo":
                page.get_by_role("button", name="撤销", exact=True).click()
                # 同一文件版本仍允许重做，再次撤销后才模拟远端更新。
                expect(page.get_by_role("button", name="重做", exact=True)).to_be_enabled()
                page.get_by_role("button", name="重做", exact=True).click()
                expect(page.get_by_text("待保存变更 · 1 项", exact=False)).to_be_visible()
                page.get_by_role("button", name="撤销", exact=True).click()
            elif clearing == "row":
                page.get_by_text("查看全部 1 项变更", exact=False).click()
                page.get_by_role("button", name="撤销此行", exact=True).click()
            elif clearing == "original":
                edit_weight("100")
            if clearing != "dirty":
                expect(page.get_by_role("button", name="放弃草稿", exact=True)).to_have_count(0)

            revision = "b" * 40
            with page.expect_response("**/api/dictionary/file?*"):
                page.get_by_role("button", name="刷新", exact=True).click()
            expect(page.get_by_text("bbbbbbbb", exact=True)).to_be_visible()
            conflict = page.get_by_text("文件已更新，当前草稿无法提交。", exact=False)
            if clearing == "dirty":
                # 有未提交变更时必须继续阻止保存，直到用户主动放弃。
                expect(conflict).to_be_visible()
                expect(page.get_by_role("button", name="保存变更（1）", exact=True)).to_be_disabled()
                expect(table).to_have_attribute("aria-readonly", "true")
                assert not posts, posts
                page.get_by_role("button", name="放弃草稿", exact=True).click()

            expect(conflict).to_have_count(0)
            expect(table).to_have_attribute("aria-readonly", "false")
            # 新文件不能重做或撤销旧版本的行号变更。
            expect(page.get_by_role("button", name="撤销", exact=True)).to_be_disabled()
            expect(page.get_by_role("button", name="重做", exact=True)).to_be_disabled()
            edit_weight("350")
            with page.expect_request("**/api/dictionary/changes"):
                page.get_by_role("button", name="保存变更（1）", exact=True).click()
            assert posts == [{"path": "packs/programming/quanpin.txt", "sha": revision, "changes": [{"operation": "replace", "line": 1, "columns": ["示例", "shi'li", "350"]}]}], posts
            expect(page.get_by_text("已创建 PR #42。", exact=False)).to_be_visible()
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
                check_dictionary_revisions(browser, f"http://127.0.0.1:{server.server_port}")
            finally:
                browser.close()
        print("词库草稿版本与冲突保护验证通过")
    finally:
        server.shutdown()
