#!/usr/bin/env python3
"""在真实浏览器中验证社区分类切换不会把旧作品绑定到新分类。"""

import json
import threading
from urllib.parse import parse_qs, urlencode, urlsplit

from playwright.sync_api import expect, sync_playwright

import csp_smoke as smoke


SECTIONS = (
    ("skins", "键盘皮肤"), ("candidate-skins", "候选窗皮肤"),
    ("plugins", "插件"), ("dictionaries", "社区词库"),
    ("replies", "回复模板"), ("phrases", "短语包"),
)


def check_community_section_switch(browser, base):
    for index, (source, _) in enumerate(SECTIONS):
        target, label = SECTIONS[(index + 1) % len(SECTIONS)]
        context = browser.new_context(viewport={"width": 1280, "height": 1000})
        page = context.new_page()
        errors, pending, posts = [], [], []
        released = False
        page.on("pageerror", lambda error: errors.append(str(error)))
        page.add_init_script("window.cspViolations = []; document.addEventListener('securitypolicyviolation', e => window.cspViolations.push(e.violatedDirective));")
        shell = smoke.FIXTURES["/api/shell"]
        page.route("**/api/shell", lambda route: route.fulfill(json={**shell, "me": {**shell["me"], "permissions": ["review_community"]}}))
        page.route("**/api/community/counts", lambda route: route.fulfill(json={section: {"pending": int(section == target and not posts), "approved": 0, "removed": 0} for section, _ in SECTIONS}))
        original = {**smoke.FIXTURES["/api/skins"]["items"][0], "id": "shared-item", "name": "原分类作品"}
        current = {**original, "name": "新分类作品"}

        def listing(route):
            section = urlsplit(route.request.url).path.removeprefix("/api/")
            if section == target and not released:
                pending.append(route)
                return
            item = current if section == target else original
            route.fulfill(json={"items": [item], "page": 1, "total": 1, "has_more": False})

        def action(route):
            posts.append(route.request.post_data_json)
            route.fulfill(json={"ok": True, "affected": 1})

        for section, _ in SECTIONS:
            page.route(f"**/api/{section}?*", listing)
        page.route("**/api/actions", action)
        try:
            page.goto(base + f"/{source}")
            expect(page.get_by_role("button", name="原分类作品", exact=True)).to_be_visible()
            page.locator(f'aside a[href="/{target}"]').click()
            expect(page.get_by_role("heading", name=label, exact=True)).to_be_visible()
            expect(page.locator(f'aside a[href="/{target}"]')).to_have_attribute("aria-current", "page")
            # 新分类响应仍被挂起，旧卡片及其审核入口必须立即消失。
            expect(page.get_by_role("button", name="原分类作品", exact=True)).to_be_hidden()
            expect(page.get_by_role("status", name="正在加载", exact=True)).to_be_visible()
            expect(page.get_by_role("button", name="通过并上架", exact=True)).to_have_count(0)
            assert pending, f"没有收到 {target} 的列表请求"
            released = True
            pending.pop().fulfill(json={"items": [current], "page": 1, "total": 1, "has_more": False})
            expect(page.get_by_role("button", name="新分类作品", exact=True)).to_be_visible()
            expect(page.locator(f'aside a[href="/{target}"]')).to_contain_text("1 项待处理")
            with page.expect_request("**/api/actions"):
                page.get_by_role("button", name="通过并上架", exact=True).click()
            assert posts[-1]["section"] == target, posts
            assert posts[-1]["id"] == current["id"], posts
            expect(page.locator(f'aside a[href="/{target}"]').get_by_text("项待处理", exact=True)).to_have_count(0)
            assert not errors, errors
            assert page.evaluate("window.cspViolations") == []
        finally:
            for route in pending:
                route.abort()
            page.unroute_all(behavior="ignoreErrors")
            context.close()


def check_community_pagination_placeholder(browser, base):
    context = browser.new_context(viewport={"width": 1280, "height": 1000})
    page = context.new_page()
    pending = []
    original = {**smoke.FIXTURES["/api/skins"]["items"][0], "name": "第一页作品"}
    current = {**original, "id": "second-page", "name": "第二页作品"}

    def listing(route):
        if parse_qs(urlsplit(route.request.url).query).get("page") == ["2"]:
            pending.append(route)
        else:
            route.fulfill(json={"items": [original], "page": 1, "total": 51, "has_more": True})

    page.route("**/api/skins?*", listing)
    try:
        page.goto(base + "/community")
        expect(page.get_by_role("button", name="第一页作品", exact=True)).to_be_visible()
        with page.expect_request("**/api/skins?page=2"):
            page.get_by_role("button", name="下一页", exact=True).click()
        expect(page.get_by_text("第 2 / 2 页 · 共 51 项", exact=True)).to_be_visible()
        # 相同分类翻页时仍保留原列表，避免修复分类切换时破坏已有的翻页体验。
        expect(page.get_by_role("button", name="第一页作品", exact=True)).to_be_visible()
        assert pending, "没有收到第二页的列表请求"
        pending.pop().fulfill(json={"items": [current], "page": 2, "total": 51, "has_more": False})
        expect(page.get_by_role("button", name="第二页作品", exact=True)).to_be_visible()
        expect(page.get_by_role("button", name="第一页作品", exact=True)).to_be_hidden()
    finally:
        for route in pending:
            route.abort()
        page.unroute_all(behavior="ignoreErrors")
        context.close()


def check_community_entry_links(browser, base):
    context = browser.new_context(viewport={"width": 1280, "height": 1000})
    page = context.new_page()
    errors, requests = [], []
    page.on("pageerror", lambda error: errors.append(str(error)))
    page.add_init_script("window.cspViolations = []; document.addEventListener('securitypolicyviolation', e => window.cspViolations.push(e.violatedDirective));")
    item = smoke.FIXTURES["/api/skins"]["items"][0]
    detail = smoke.FIXTURES["/api/skins/smoke-skin"]

    def listing(route):
        requests.append(urlsplit(route.request.url).path)
        route.fulfill(json={"items": [item], "page": 2, "total": 101, "has_more": True})

    for section, _ in SECTIONS:
        page.route(f"**/api/{section}?*", listing)
        page.route(f"**/api/{section}/smoke-skin", lambda route: route.fulfill(json=detail))
    try:
        for section, label in SECTIONS:
            requests.clear()
            query = {"tab": section, "status": "pending", "page": "2", "visibility": "private"}
            page.goto(base + "/community?" + urlencode(query))
            expect(page.get_by_role("heading", name=label, exact=True)).to_be_visible()
            expect(page.get_by_role("button", name=item["name"], exact=True)).to_be_visible()
            location = urlsplit(page.url)
            assert location.path == f"/{section}", page.url
            params = parse_qs(location.query)
            assert str(json.loads(params.pop("page")[0])) == "2", page.url
            assert params == {"status": ["pending"], "visibility": ["private"]}, page.url
            assert requests == [f"/api/{section}"], requests
            expect(page.locator(f'aside a[href="/{section}"]')).to_have_attribute("aria-current", "page")
            expect(page.get_by_role("radiogroup", name="内容类型")).to_have_count(0)

        page.goto(base + "/community?tab=skins&focus=plugins%2Fsmoke-skin")
        expect(page.get_by_role("dialog", name=item["name"], exact=True)).to_be_visible()
        expect(page.get_by_role("heading", name="插件", exact=True, include_hidden=True)).to_have_count(1)
        assert urlsplit(page.url).path == "/plugins", page.url
        page.keyboard.press("Escape")

        # 后端旧版搜索与通知仍发送 community 目标，前端按作品前缀直达。
        page.route("**/api/search?*", lambda route: route.fulfill(json={"items": [{"kind": "community", "id": "plugins/smoke-skin", "title": "定位插件", "where": "社区", "target": "community"}]}))
        page.goto(base + "/")
        page.get_by_placeholder("搜索页面、PR、Issue、用户…").fill("定位")
        page.get_by_role("option", name="定位插件 社区", exact=True).click()
        expect(page.get_by_role("dialog", name=item["name"], exact=True)).to_be_visible()
        expect(page.get_by_role("heading", name="插件", exact=True, include_hidden=True)).to_have_count(1)
        page.keyboard.press("Escape")
        page.route("**/api/notifications?*", lambda route: route.fulfill(json={"items": [{"id": 2, "kind": "community", "title": "定位短语", "target_page": "community", "target_id": "phrases/smoke-skin", "created_at": "2026-10-01T00:00:00Z", "read": True}], "unread": 0}))
        page.get_by_role("button", name="通知，1 条未读", exact=True).click()
        page.get_by_role("button", name="定位短语", exact=False).click()
        expect(page.get_by_role("dialog", name=item["name"], exact=True)).to_be_visible()
        expect(page.get_by_role("heading", name="短语包", exact=True, include_hidden=True)).to_have_count(1)
        page.keyboard.press("Escape")

        page.set_viewport_size({"width": 390, "height": 800})
        for section, label in SECTIONS:
            page.get_by_role("button", name="打开导航", exact=True).click()
            page.locator(f'aside a[href="/{section}"]').click()
            expect(page.get_by_role("heading", name=label, exact=True)).to_be_visible()
            expect(page.locator("#admin-navigation")).to_have_attribute("inert", "")
            assert page.evaluate("document.documentElement.scrollWidth <= innerWidth"), section
        assert not errors, errors
        assert page.evaluate("window.cspViolations") == []
    finally:
        context.close()


if __name__ == "__main__":
    smoke.Handler.csp = smoke.production_csp()
    server = smoke.Server(("127.0.0.1", 0), smoke.Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    try:
        with sync_playwright() as playwright:
            browser = playwright.chromium.launch()
            base = f"http://127.0.0.1:{server.server_port}"
            check_community_entry_links(browser, base)
            check_community_section_switch(browser, base)
            check_community_pagination_placeholder(browser, base)
            browser.close()
        print("社区分类切换和同分类翻页验证通过")
    finally:
        server.shutdown()
