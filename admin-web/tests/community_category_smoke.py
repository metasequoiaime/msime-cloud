#!/usr/bin/env python3
"""在生产 CSP 下验证分类修改固定页面所见的分类，撤销使用服务端版本。"""

import threading

from playwright.sync_api import expect, sync_playwright

import csp_smoke as smoke


def check_community_categories(browser, base):
    for section in ("skins", "candidate-skins"):
        for concurrent_change in ("before_change", "before_undo", "roundtrip", "none", "missing", "invalid"):
            context = browser.new_context(viewport={"width": 1280, "height": 1000})
            page = context.new_page()
            page.clock.install()
            errors = []
            page.on("pageerror", lambda error: errors.append(str(error)))
            page.add_init_script("window.cspViolations = []; document.addEventListener('securitypolicyviolation', e => window.cspViolations.push(e.violatedDirective));")
            shell = smoke.FIXTURES["/api/shell"]
            page.route("**/api/auth/session", lambda route: route.fulfill(json=smoke.FIXTURES["/api/auth/session"]))
            page.route("**/api/shell", lambda route: route.fulfill(json={**shell, "me": {**shell["me"], "permissions": ["review_community"]}}))
            item = {**smoke.FIXTURES["/api/skins"]["items"][0], "category": "other", "visibility": "public", "version": "1.0", "size": 1, "file_count": 1}
            detail = {**smoke.FIXTURES["/api/skins/smoke-skin"], "category": "other"}
            page.route(f"**/api/{section}?*", lambda route: route.fulfill(json={"items": [item], "page": 1, "total": 1, "has_more": False}))
            page.route(f"**/api/{section}/smoke-skin", lambda route: route.fulfill(json=detail))
            posts = []
            version = {"token": "a" * 32}
            undo = {"action": "set_skin_category" if section == "skins" else "set_candidate_skin_category", "section": section, "id": "smoke-skin", "value": {"category": "other", "token": version["token"]}}

            def action(route):
                body = route.request.post_data_json
                posts.append(body)
                assert body["action"] == ("set_skin_category" if section == "skins" else "set_candidate_skin_category")
                value = body["value"]
                # 首次操作比较 expected；撤销还必须匹配写入版本，状态往返也会使版本失效。
                if ("expected" in value and value["expected"] != detail["category"]) or ("token" in value and value["token"] != version["token"]):
                    route.fulfill(status=409, json={"error": {"code": "conflict"}})
                else:
                    item["category"] = detail["category"] = value["category"]
                    result = {"ok": True, "affected": 1}
                    if len(posts) == 1 and concurrent_change != "missing":
                        result["undo"] = undo if concurrent_change != "invalid" else {**undo, "id": "another-item"}
                    route.fulfill(json=result)

            page.route("**/api/actions", action)
            try:
                page.goto(base + f"/community?tab={section}&focus=smoke-skin")
                category = page.get_by_role("dialog", name="春日樱", exact=True).get_by_label("分类", exact=True)
                expect(category).to_have_value("other")
                if concurrent_change == "before_change":
                    # 页面仍显示 other，另一位管理员已经改成 food。
                    item["category"] = detail["category"] = "food"
                category.select_option("cute")
                if concurrent_change == "before_change":
                    expect(page.get_by_text("操作失败：该内容刚被其他审核员处理或被作者修改，未做改动，请查看最新内容后重试。", exact=True)).to_be_visible()
                    expect(category).to_have_value("food")
                    expect(page.get_by_role("button", name="撤销", exact=True, include_hidden=True)).to_have_count(0)
                    assert detail["category"] == "food", "旧页面覆盖了另一位管理员的分类"
                    assert posts[0]["value"] == {"category": "cute", "expected": "other"}
                else:
                    expect(category).to_have_value("cute")
                    if concurrent_change in ("missing", "invalid"):
                        expect(page.get_by_role("button", name="撤销", exact=True, include_hidden=True)).to_have_count(0)
                        assert len(posts) == 1, posts
                        assert not errors, errors
                        assert page.evaluate("window.cspViolations") == []
                        continue
                    if concurrent_change in ("before_undo", "roundtrip"):
                        item["category"] = detail["category"] = "food" if concurrent_change == "before_undo" else "cute"
                        version["token"] = "b" * 32
                    page.get_by_role("button", name="撤销", exact=True, include_hidden=True).click()
                    if concurrent_change in ("before_undo", "roundtrip"):
                        expect(page.get_by_text("撤销失败：该内容刚被其他审核员处理或被作者修改，未做改动，请查看最新内容后重试。", exact=True)).to_be_visible()
                        expected = "food" if concurrent_change == "before_undo" else "cute"
                        expect(category).to_have_value(expected)
                        assert detail["category"] == expected, "过期撤销覆盖了另一位管理员的分类"
                    else:
                        expect(page.get_by_text("已撤销", exact=True)).to_be_visible()
                        expect(category).to_have_value("other")
                        assert detail["category"] == "other"
                    assert [post["value"] for post in posts] == [{"category": "cute", "expected": "other"}, undo["value"]]
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
            check_community_categories(browser, f"http://127.0.0.1:{server.server_port}")
            browser.close()
        print("社区分类浏览器验证通过")
    finally:
        server.shutdown()
