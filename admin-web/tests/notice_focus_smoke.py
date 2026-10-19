#!/usr/bin/env python3
"""验证公告跳转等待新列表，切换草稿先确认，并在刷新后保留本地输入。"""

import threading
from urllib.parse import unquote

from playwright.sync_api import expect, sync_playwright

import csp_smoke as smoke


def check_notice_focus_refresh(browser, base):
    for status in ("draft", "live", "archived"):
        context = browser.new_context(viewport={"width": 1280, "height": 1000})
        page = context.new_page()
        errors, pending = [], []
        page.on("pageerror", lambda error: errors.append(str(error)))
        page.add_init_script("window.cspViolations = []; document.addEventListener('securitypolicyviolation', event => window.cspViolations.push(event.violatedDirective));")
        page.clock.install()
        fixture = smoke.FIXTURES["/api/notices"]
        target = {**fixture["items"][1], "id": "4", "title": f"新到的{status}公告", "body": "刷新后才收到的正文", "status": status}
        other_draft = {**fixture["items"][1], "id": "5", "title": f"另一条{status}草稿", "body": "另一条草稿正文", "status": "draft"}
        if status != "draft":
            target["published_at"] = "2026-10-01T00:00:00Z"
        loaded = False
        refreshed = None

        def listing(route):
            nonlocal loaded
            if not loaded:
                loaded = True
                route.fulfill(json=fixture)
            elif refreshed is not None:
                route.fulfill(json=refreshed)
            else:
                pending.append(route)

        page.route("**/api/notices", listing)
        page.route("**/api/auth/session", lambda route: route.fulfill(json=smoke.FIXTURES["/api/auth/session"]))
        def search(route):
            hit = other_draft if other_draft["title"] in unquote(route.request.url) else target
            route.fulfill(json={"items": [{"kind": "notice", "id": hit["id"], "title": hit["title"], "where": "公告", "target": "notice"}]})

        page.route("**/api/search?*", search)
        page.route("**/api/actions", lambda route: route.fulfill(status=500, json={"error": {"code": "internal_error", "message": "internal_error"}}))
        try:
            page.goto(base + "/notice")
            expect(page.locator("#notice-2")).to_be_visible()
            page.locator('a[href="/users"]').first.click()
            expect(page).to_have_url(base + "/users")
            # 让缓存超过实际的 30 秒新鲜期，返回公告页时先显示旧列表并刷新。
            page.clock.fast_forward(31_000)
            page.get_by_role("combobox", name="全局搜索").fill(target["title"])
            page.get_by_role("option", name=target["title"] + " 公告", exact=True).click()
            expect(page).to_have_url(base + '/notice?focus=%224%22')
            expect(page.locator("#notice-2")).to_be_visible()
            # 等待旧列表完成绘制及其 effect，随后才让新列表返回。
            page.evaluate("() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)))")
            assert len(pending) == 1, "返回公告页时没有刷新过期列表"
            pending.pop().fulfill(json={**fixture, "items": [*fixture["items"], target, other_draft]})
            row = page.locator("#notice-4")
            expect(row).to_be_visible()
            expect(row.locator("button[aria-expanded]")).to_have_attribute("aria-expanded", "true")
            expect(row.get_by_text(target["body"], exact=True)).to_be_visible()
            if status == "draft":
                title = page.get_by_role("textbox", name="标题", exact=True)
                expect(title).to_have_value(target["title"])
                body = page.get_by_role("textbox", name="内容", exact=True)
                expect(body).to_have_value(target["body"])
                title.fill("尚未保存的本地标题")
                body.fill("尚未保存的本地正文")
                page.get_by_role("combobox", name="全局搜索").fill(other_draft["title"])
                page.get_by_role("option", name=other_draft["title"] + " 公告", exact=True).click()
                confirm = page.get_by_role("dialog")
                expect(confirm).to_be_visible()
                expect(confirm).to_contain_text("放弃未保存的修改？")
                confirm.get_by_role("button", name="取消", exact=True).click()
                expect(page).to_have_url(base + "/notice")
                expect(title).to_have_value("尚未保存的本地标题")
                expect(body).to_have_value("尚未保存的本地正文")
                # 再次定位正在编辑的同一条草稿，只展开记录，不重置本地内容。
                page.get_by_role("combobox", name="全局搜索").fill(target["title"])
                page.get_by_role("option", name=target["title"] + " 公告", exact=True).click()
                expect(page).to_have_url(base + '/notice?focus=%224%22')
                expect(page.get_by_role("dialog")).not_to_be_visible()
                expect(title).to_have_value("尚未保存的本地标题")
                expect(body).to_have_value("尚未保存的本地正文")
                page.get_by_role("combobox", name="全局搜索").fill(other_draft["title"])
                page.get_by_role("option", name=other_draft["title"] + " 公告", exact=True).click()
                page.get_by_role("dialog").get_by_role("button", name="放弃修改", exact=True).click()
                expect(title).to_have_value(other_draft["title"])
                expect(body).to_have_value(other_draft["body"])
                # 未修改时可以直接切换回另一条草稿。
                page.get_by_role("combobox", name="全局搜索").fill(target["title"])
                page.get_by_role("option", name=target["title"] + " 公告", exact=True).click()
                expect(title).to_have_value(target["title"])
                title.fill("尚未保存的本地标题")
                body.fill("尚未保存的本地正文")
                # 保存失败仍会刷新列表；同一次已完成的跳转不能重新载入远端草稿。
                updated = {**target, "title": "远端标题已更新", "body": "远端正文已更新"}
                refreshed = {**fixture, "items": [*fixture["items"], updated]}
                with page.expect_response("**/api/notices"):
                    page.get_by_role("button", name="存为草稿", exact=True).click()
                expect(row.locator("button[aria-expanded]")).to_contain_text(updated["title"])
                expect(title).to_be_enabled()
                expect(title).to_have_value("尚未保存的本地标题")
                expect(body).to_have_value("尚未保存的本地正文")
            assert not errors, errors
            assert page.evaluate("window.cspViolations") == []
        finally:
            for route in pending:
                route.abort()
            page.unroute_all(behavior="ignoreErrors")
            context.close()


def check_notice_focus_unsaved(browser, base):
    for initial in ("", "?focus=3"):
        for status in ("draft", "live", "archived"):
            context = browser.new_context(viewport={"width": 1280, "height": 1000})
            page = context.new_page()
            errors, pending = [], []
            page.on("pageerror", lambda error: errors.append(str(error)))
            page.add_init_script("window.cspViolations = []; document.addEventListener('securitypolicyviolation', event => window.cspViolations.push(event.violatedDirective));")
            fixture = smoke.FIXTURES["/api/notices"]
            target = {**fixture["items"][1], "id": "4", "title": "待打开的公告", "body": "服务端正文", "status": status}
            page.route("**/api/auth/session", lambda route: route.fulfill(json=smoke.FIXTURES["/api/auth/session"]))
            page.route("**/api/notices", lambda route: route.fulfill(json={**fixture, "items": [*fixture["items"], target]}))
            page.route("**/api/search?*", lambda route: route.fulfill(json={"items": [{"kind": "notice", "id": target["id"], "title": target["title"], "where": "公告", "target": "notice"}]}))
            page.route("**/api/actions", lambda route: pending.append(route))
            try:
                page.goto(base + "/notice" + initial)
                title = page.locator("#notice-title")
                body = page.locator("#notice-body")
                expect(title).to_have_value(fixture["items"][1]["title"] if initial else "")
                title.fill("保留本地标题")
                body.fill("保留本地正文")
                if initial and status == "draft":
                    # 写入失败后仍要确认切换，不能把尚未保存的输入当作已提交内容。
                    with page.expect_request("**/api/actions"):
                        page.get_by_role("button", name="存为草稿", exact=True).click()
                    expect(title).to_be_disabled()
                page.get_by_role("combobox", name="全局搜索").fill(target["title"])
                page.get_by_role("option", name=target["title"] + " 公告", exact=True).click()
                if pending:
                    expect(page.get_by_role("dialog")).not_to_be_visible()
                    pending.pop().fulfill(status=500, json={"error": {"code": "internal_error", "message": "internal_error"}})
                if status == "draft":
                    dialog = page.get_by_role("dialog")
                    expect(dialog).to_contain_text("放弃未保存的修改？")
                    expect(title).to_have_value("保留本地标题")
                    expect(body).to_have_value("保留本地正文")
                    dialog.get_by_role("button", name="放弃修改", exact=True).click()
                    expect(title).to_have_value(target["title"])
                    expect(body).to_have_value(target["body"])
                else:
                    expect(page.locator("#notice-4 button[aria-expanded]")).to_have_attribute("aria-expanded", "true")
                    expect(page.get_by_role("dialog")).not_to_be_visible()
                    expect(title).to_have_value("保留本地标题")
                    expect(body).to_have_value("保留本地正文")
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
    try:
        with sync_playwright() as playwright:
            browser = playwright.chromium.launch()
            try:
                check_notice_focus_refresh(browser, f"http://127.0.0.1:{server.server_port}")
                check_notice_focus_unsaved(browser, f"http://127.0.0.1:{server.server_port}")
                print("公告缓存刷新和未保存修改跳转回归通过")
            finally:
                browser.close()
    finally:
        server.shutdown()
        server.server_close()
