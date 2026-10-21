#!/usr/bin/env python3
"""在后台 CSP 下验证社区管理的表单、权限、保存冲突和删除撤销。"""

import base64
import json
import threading
import time

from playwright.sync_api import expect, sync_playwright

import csp_smoke as smoke


def check_community_management(browser, base):
    context = browser.new_context(viewport={"width": 1280, "height": 1000})
    page = context.new_page()
    errors = []
    page.on("pageerror", lambda error: errors.append(str(error)))
    page.add_init_script("window.cspViolations = []; document.addEventListener('securitypolicyviolation', e => window.cspViolations.push(e.violatedDirective));")
    shell = smoke.FIXTURES["/api/shell"]
    permissions = ["review_community"]
    page.route("**/api/auth/session", lambda route: route.fulfill(json=smoke.FIXTURES["/api/auth/session"]))
    page.route("**/api/shell", lambda route: route.fulfill(json={**shell, "me": {**shell["me"], "permissions": permissions}}))
    sections = ("skins", "plugins", "candidate-skins")
    items = {section: [] for section in sections}
    details = {}
    requests = []
    posts = []
    pending = []

    def wait_pending():
        deadline = time.monotonic() + 10
        while not pending and time.monotonic() < deadline:
            page.wait_for_timeout(10)
        assert pending, "没有收到预期的写入请求"

    def listing(route):
        section = route.request.url.split("/api/")[1].split("?")[0]
        requests.append(route.request.url)
        route.fulfill(json={"items": items[section], "page": 1, "total": len(items[section]), "has_more": False})

    def detail(route):
        section, item_id = route.request.url.split("/api/")[1].split("/")
        route.fulfill(json=details[(section, item_id)])

    def action(route):
        posts.append(route.request.post_data_json)
        pending.append(route)

    def commit(route):
        body = route.request.post_data_json
        section, item_id = body["section"], body["id"]
        if body["action"].startswith("delete_"):
            items[section] = [item for item in items[section] if item["id"] != item_id]
            details.pop((section, item_id), None)
        else:
            value = body["value"]
            item = {**smoke.FIXTURES["/api/skins"]["items"][0], "id": item_id, "name": value["name"], "description": value["description"], "moderation": value["moderation"], "edit_token": "current-token"}
            items[section] = [item]
            details[(section, item_id)] = {**smoke.FIXTURES["/api/skins/smoke-skin"], **item, "reports": [], "content": value.get("design", "manifest"), "edit_token": "current-token"}
        route.fulfill(json={"ok": True, "affected": 1, "id": item_id})

    for section in sections:
        page.route(f"**/api/{section}?*", listing)
        page.route(f"**/api/{section}/*", detail)
    page.route("**/api/actions", action)
    try:
        page.goto(base + "/community")
        expect(page.get_by_role("heading", name="社区管理", exact=True)).to_be_visible()
        expect(page.get_by_role("radio", name="全部", exact=True)).to_have_attribute("data-state", "on")
        page.get_by_role("button", name="新增键盘皮肤", exact=True).click()
        form = page.get_by_role("dialog", name="新增键盘皮肤", exact=True)
        form.get_by_label("作者账号", exact=True).select_option("u1")
        form.get_by_label("名称", exact=True).fill("管理测试皮肤")
        form.get_by_label("背景颜色", exact=True).fill("#abcdef")
        form.get_by_role("button", name="保存", exact=True).click()
        expect(form.get_by_role("button", name="保存中…")).to_be_disabled()
        expect(form.get_by_label("名称", exact=True)).to_be_disabled()
        expect(form.get_by_role("button", name="取消", exact=True)).to_be_disabled()
        page.keyboard.press("Escape")
        expect(form).to_be_visible()
        wait_pending()
        assert posts[-1]["value"]["design"]["background"] == 0xABCDEF
        assert posts[-1]["value"]["moderation"] == "removed"
        pending.pop().fulfill(status=409, json={"error": {"code": "content_changed"}})
        expect(form.get_by_role("alert")).to_contain_text("作品已被修改")
        expect(form.get_by_label("名称", exact=True)).to_have_value("管理测试皮肤")
        form.get_by_role("button", name="保存", exact=True).click()
        expect(form.get_by_role("button", name="保存中…")).to_be_disabled()
        wait_pending()
        assert posts[-1]["id"] == posts[-2]["id"]
        commit(pending.pop())
        expect(form).to_be_hidden()
        drawer = page.get_by_role("dialog", name="管理测试皮肤", exact=True)
        drawer.get_by_role("button", name="编辑", exact=True).click()
        form = page.get_by_role("dialog", name="编辑键盘皮肤", exact=True)
        form.get_by_label("简介", exact=True).fill("保留作者的编辑")
        form.get_by_label("发布状态", exact=True).select_option("approved")
        form.get_by_role("button", name="保存", exact=True).click()
        expect(form.get_by_role("button", name="保存中…")).to_be_disabled()
        wait_pending()
        assert posts[-1]["action"] == "update_skin"
        assert posts[-1]["value"]["edit_token"] == "current-token"
        assert posts[-1]["value"]["owner_id"] == "u1"
        commit(pending.pop())
        expect(form).to_be_hidden()
        drawer.get_by_role("button", name="删除", exact=True).click()
        confirmation = page.get_by_role("dialog", name="删除「管理测试皮肤」？", exact=True)
        confirmation.get_by_role("button", name="删除", exact=True).click()
        page.get_by_role("button", name="撤销", exact=True).click()
        page.wait_for_timeout(4200)
        assert not any(post["action"] == "delete_skin" for post in posts)
        page.get_by_role("button", name="管理测试皮肤", exact=True).click()
        drawer.get_by_role("button", name="删除", exact=True).click()
        confirmation.get_by_role("button", name="删除", exact=True).click()
        page.wait_for_timeout(4200)
        wait_pending()
        assert posts[-1]["action"] == "delete_skin"
        assert posts[-1].get("value", {}).get("edit_token") == "current-token", "删除请求没有固定确认时的作品版本"
        item_id = posts[-1]["id"]
        items["skins"][0]["edit_token"] = "new-token"
        details[("skins", item_id)]["edit_token"] = "new-token"
        pending.pop().fulfill(status=409, json={"error": {"code": "conflict"}})
        expect(page.get_by_text("操作失败：该内容刚被其他审核员处理或被作者修改，未做改动，请查看最新内容后重试。", exact=True)).to_be_visible()
        expect(page.get_by_role("button", name="管理测试皮肤", exact=True)).to_be_visible()

        # 缺少版本的旧响应不能让界面发出不受保护的删除。
        del items["skins"][0]["edit_token"]
        del details[("skins", item_id)]["edit_token"]
        page.reload()
        page.get_by_role("button", name="管理测试皮肤", exact=True).click()
        drawer.get_by_role("button", name="删除", exact=True).click()
        expect(page.get_by_text("作品版本信息尚未加载，请刷新页面后重试。", exact=True)).to_be_visible()
        expect(confirmation).to_be_hidden()
        assert not pending, "缺少版本时发送了删除请求"
        items["skins"][0]["edit_token"] = "new-token"
        details[("skins", item_id)]["edit_token"] = "new-token"
        page.reload()
        page.get_by_role("button", name="管理测试皮肤", exact=True).click()
        drawer.get_by_role("button", name="删除", exact=True).click()
        confirmation.get_by_role("button", name="删除", exact=True).click()
        wait_pending()
        assert posts[-1]["value"]["edit_token"] == "new-token"
        commit(pending.pop())
        expect(page.get_by_role("button", name="管理测试皮肤", exact=True)).to_be_hidden()

        for tab, label, archive_label in [("plugins", "插件", "插件 ZIP 包"), ("candidate-skins", "候选窗皮肤", "皮肤图片")]:
            page.goto(base + f"/community?tab={tab}")
            page.get_by_role("button", name=f"新增{label}", exact=True).click()
            form = page.get_by_role("dialog", name=f"新增{label}", exact=True)
            form.get_by_label("作者账号", exact=True).select_option("u1")
            form.get_by_label("名称", exact=True).fill(f"管理测试{label}")
            payload = b"synthetic archive" if tab == "plugins" else b"synthetic png"
            form.get_by_label(archive_label, exact=True).set_input_files({"name": "plugin.zip" if tab == "plugins" else "preview.png", "mimeType": "application/octet-stream", "buffer": payload})
            if tab == "candidate-skins":
                form.get_by_label("皮肤清单（skin.toml）", exact=True).fill("synthetic manifest")
                form.get_by_label("preview.png 的包内路径", exact=True).fill("assets/preview.png")
            form.get_by_role("button", name="保存", exact=True).click()
            expect(form.get_by_role("button", name="保存中…")).to_be_disabled()
            wait_pending()
            value = posts[-1]["value"]
            encoded = value["archive"] if tab == "plugins" else value["files"]["assets/preview.png"]
            assert base64.b64decode(encoded) == payload
            pending.pop().fulfill(status=400, json={"error": {"code": "invalid_value"}})
            expect(form.get_by_label("名称", exact=True)).to_be_enabled()
            expect(form.get_by_label("名称", exact=True)).to_have_value(f"管理测试{label}")
            form.get_by_role("button", name="取消", exact=True).click()

        assert all("status=" not in url for url in requests)
        page.goto(base + "/community?status=pending")
        expect(page.get_by_role("radio", name="待审核", exact=True)).to_have_attribute("data-state", "on")
        expect(page.get_by_text("暂无待审核内容，可切换到全部查看已有作品", exact=True)).to_be_visible()
        assert "status=pending" in requests[-1]
        permissions.clear()
        page.goto(base + "/community")
        expect(page.get_by_role("button", name="新增键盘皮肤", exact=True)).to_be_disabled()
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
            check_community_management(browser, f"http://127.0.0.1:{server.server_port}")
            browser.close()
        print("社区管理浏览器验证通过")
    finally:
        server.shutdown()
