#!/usr/bin/env python3
"""验证审核撤销使用服务端实际快照，且旧版本响应不提供不安全撤销。"""

import threading

from playwright.sync_api import expect, sync_playwright

import csp_smoke as smoke


def check_community_undo(browser, base):
    for action, button, initial in (
        ("approve_content", "通过并上架", "pending"),
        ("remove_content", "下架", "approved"),
        ("restore_content", "恢复", "removed"),
    ):
        for scenario in ("success", "conflict", "missing", "invalid"):
            context = browser.new_context(viewport={"width": 1280, "height": 1000})
            page = context.new_page()
            page.clock.install()
            errors, posts = [], []
            page.on("pageerror", lambda error: errors.append(str(error)))
            page.add_init_script("window.cspViolations = []; document.addEventListener('securitypolicyviolation', e => window.cspViolations.push(e.violatedDirective));")
            shell = smoke.FIXTURES["/api/shell"]
            page.route("**/api/auth/session", lambda route: route.fulfill(json=smoke.FIXTURES["/api/auth/session"]))
            page.route("**/api/shell", lambda route: route.fulfill(json={**shell, "me": {**shell["me"], "permissions": ["review_community"]}}))
            item = {**smoke.FIXTURES["/api/skins"]["items"][0], "moderation": initial}
            detail = {**smoke.FIXTURES["/api/skins/smoke-skin"], "moderation": initial}
            page.route("**/api/skins?*", lambda route: route.fulfill(json={"items": [item], "page": 1, "total": 1, "has_more": False}))
            page.route("**/api/skins/smoke-skin", lambda route: route.fulfill(json=detail))
            # 下架时服务器已是 removed，与旧页面的 approved 不同；其余操作的实际原因也与页面不同。
            snapshot = {"token": "a" * 32, "moderation": "removed" if action == "remove_content" else initial, "previous": "pending" if action != "approve_content" else None, "reason": "服务器保存的原始原因"}
            undo = {"action": "undo_moderation", "section": "skins", "id": "smoke-skin", "value": snapshot}

            def respond(route):
                body = route.request.post_data_json
                posts.append(body)
                if len(posts) == 1:
                    assert body["action"] == action, body
                    item["moderation"] = detail["moderation"] = "removed" if action == "remove_content" else "approved"
                    result = {"ok": True, "affected": 1}
                    if scenario != "missing":
                        result["undo"] = undo if scenario != "invalid" else {**undo, "id": "another-item"}
                    route.fulfill(json=result)
                elif scenario == "conflict":
                    item["moderation"] = detail["moderation"] = "removed"
                    route.fulfill(status=409, json={"error": {"code": "conflict"}})
                else:
                    item["moderation"] = detail["moderation"] = snapshot["moderation"]
                    route.fulfill(json={"ok": True, "affected": 1})

            page.route("**/api/actions", respond)
            try:
                page.goto(base + "/community?tab=skins&focus=smoke-skin")
                page.get_by_role("dialog", name="春日樱", exact=True).get_by_role("button", name=button, exact=True).click()
                if action == "remove_content":
                    page.get_by_role("dialog", name="下架「春日樱」？", exact=True).get_by_role("button", name="下架", exact=True).click()
                text = "「春日樱」已下架：" if action == "remove_content" else "「春日樱」已上架" if action == "approve_content" else "「春日樱」已恢复"
                expect(page.locator("[data-msime-toast]").get_by_text(text, exact=action != "remove_content")).to_be_visible()
                undo_button = page.get_by_role("button", name="撤销", exact=True, include_hidden=True)
                if scenario in ("missing", "invalid"):
                    expect(undo_button).to_have_count(0)
                    assert len(posts) == 1, posts
                else:
                    undo_button.click()
                    message = "撤销失败：该内容刚被其他审核员处理或被作者修改，未做改动，请查看最新内容后重试。" if scenario == "conflict" else "已撤销"
                    expect(page.get_by_text(message, exact=True)).to_be_visible()
                    assert posts[1] == undo, posts
                    state = "removed" if scenario == "conflict" else snapshot["moderation"]
                    expect(page.get_by_role("article").get_by_role("button", name="恢复" if state == "removed" else "驳回", exact=True)).to_be_visible()
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
                check_community_undo(browser, f"http://127.0.0.1:{server.server_port}")
                print("社区审核撤销浏览器验证通过")
            finally:
                browser.close()
    finally:
        server.shutdown()
        server.server_close()
