#!/usr/bin/env python3
"""在生产 CSP 下验证皮肤纹理、键帽、材质和照片裁切。"""

import base64
import json
import threading
from urllib.parse import parse_qs, urlparse

from playwright.sync_api import expect, sync_playwright

import csp_smoke as smoke


def pixels(page, artwork, points):
    encoded = base64.b64encode(artwork.screenshot()).decode()
    return page.evaluate("""async ({encoded, points}) => {
        const image = new Image();
        image.src = 'data:image/png;base64,' + encoded;
        await image.decode();
        const canvas = document.createElement('canvas');
        canvas.width = image.width; canvas.height = image.height;
        const context = canvas.getContext('2d');
        context.drawImage(image, 0, 0);
        return points.map(([x, y]) => Array.from(context.getImageData(x, y, 1, 1).data).slice(0, 3));
    }""", {"encoded": encoded, "points": points})


def check_skin_preview(browser, base):
    context = browser.new_context(viewport={"width": 1280, "height": 1000})
    page = context.new_page()
    errors, preview_calls, detail_calls = [], [], []
    page.on("pageerror", lambda error: errors.append(str(error)))
    page.add_init_script("window.cspViolations = []; document.addEventListener('securitypolicyviolation', e => window.cspViolations.push(e.violatedDirective));")
    shell = smoke.FIXTURES["/api/shell"]
    page.route("**/api/auth/session", lambda route: route.fulfill(json=smoke.FIXTURES["/api/auth/session"]))
    page.route("**/api/shell", lambda route: route.fulfill(json={**shell, "me": {**shell["me"], "permissions": ["review_community"]}}))
    design = {**smoke.SMOKE_DESIGN, "background": 0x102030, "keyBackground": 0xFFFFFF, "accent": 0xFF0000, "gradientEnd": None, "cornerRadius": 0}
    item = {**smoke.FIXTURES["/api/skins"]["items"][0], "design": {**design, "keyShape": "ticket", "pattern": 1}}
    detail = {**smoke.FIXTURES["/api/skins/smoke-skin"], "content": item["design"]}
    page.route("**/api/skins/smoke-skin", lambda route: (detail_calls.append(route.request.url), route.fulfill(json=detail)))
    page.route("**/api/skins/smoke-skin?view=preview", lambda route: (preview_calls.append(route.request.url), route.fulfill(json={"content": detail["content"]})))
    page.route("**/api/skins?*", lambda route: route.fulfill(json={"items": [item], "page": 1, "total": 1, "has_more": False}))
    try:
        page.goto(base + "/community")
        card = page.get_by_role("button", name=f"查看「{item['name']}」详情")
        expect(card.locator("svg > defs pattern circle")).to_have_count(1)
        assert "A" in card.locator('[data-key="Q"] clipPath path').get_attribute("d")
        # 无照片设计直接使用列表数据；照片版本只请求轻量设计，打开抽屉才读完整详情。
        assert not preview_calls and not detail_calls
        photo = page.evaluate("""() => {
            const canvas = document.createElement('canvas'); canvas.width = canvas.height = 10;
            const ctx = canvas.getContext('2d'); ctx.fillStyle = '#123456'; ctx.fillRect(0, 0, 10, 10);
            return canvas.toDataURL('image/jpeg').split(',')[1];
        }""")
        item["design"] = {**item["design"], "photoPosition": .3, "photoShade": .25}
        detail["content"] = {**item["design"], "photo": photo}
        item["edit_token"] = detail["edit_token"] = "photo-revision"
        page.get_by_role("radio", name="待审核", exact=True).click()
        expect(card.locator("foreignObject img")).to_have_js_property("naturalWidth", 10)
        assert len(preview_calls) == 1 and not detail_calls
        page.get_by_role("button", name=item["name"], exact=True).click()
        drawer = page.get_by_role("dialog", name=item["name"], exact=True)
        expect(drawer.locator("foreignObject img")).to_have_js_property("naturalWidth", 10)
        assert len(detail_calls) == 1
        page.keyboard.press("Escape")
        page.get_by_role("button", name="新增键盘皮肤", exact=True).click()
        form = page.get_by_role("dialog", name="新增键盘皮肤", exact=True)
        form.locator("summary").click()
        editor = form.get_by_label("皮肤设计", exact=True)
        artwork = form.get_by_role("img", name="新皮肤，26 键键盘效果")

        def render(**changes):
            editor.fill(json.dumps({**design, **changes}))
            expect(artwork).to_be_visible()
            artwork.evaluate("element => { element.style.width = '390px'; element.style.maxWidth = 'none'; }")
            return artwork

        # 在实际截图上检查票券缺口与卵石轮廓，避免只检查 SVG 标签而漏掉绘制差异。
        render(keyShape="ticket")
        notch, face = pixels(page, artwork, [(8, 95), (14, 95)])
        assert notch == [16, 32, 48], notch
        assert face == [255, 255, 255], face
        render(keyShape="pebble", keyMaterial="glass")
        corner, face = pixels(page, artwork, [(8, 75), (23, 82)])
        assert corner == [16, 32, 48], corner
        assert min(face) > 245, face

        render(pattern=1)
        expect(artwork.locator("defs pattern circle")).to_have_count(1)
        expect(artwork.locator('rect[fill^="url"][opacity]')).to_have_attribute("opacity", "0.15")
        render(pattern=2)
        expect(artwork.locator("defs pattern")).to_have_attribute("width", "20")
        expect(artwork.locator("defs pattern path")).to_have_attribute("stroke-width", ".5")
        render(pattern=3)
        curves = artwork.locator('path[stroke-width="2"][opacity="0.15"]')
        assert curves.get_attribute("d").count("C") > 20
        expect(artwork.locator('rect[fill^="url"][opacity]')).to_have_count(0)

        render(borderWidth=2, keyOpacity=.25, actionBackground=0x888888)
        normal, action = pixels(page, artwork, [(23, 80), (310, 230)])
        assert all(abs(actual - expected) <= 1 for actual, expected in zip(normal, [76, 88, 100])), normal
        assert action == [136, 136, 136], action
        expect(artwork.locator('[data-key="Q"] path[stroke-width="2"]')).to_have_attribute("stroke", "#ff0000")
        expect(artwork.locator('[data-key="换行"] > svg')).to_have_attribute("stroke", "#000000")
        render(borderWidth=1, customBorderColor=0xABCDEF, keyMaterial="paper")
        expect(artwork.locator('[data-key="Q"] path[stroke-width="1"]')).to_have_attribute("stroke", "#abcdef")
        fibers = artwork.locator('[data-key="Q"] path[stroke-width=".5"]')
        assert fibers.get_attribute("d").count("M") >= 9
        render(keyMaterial="raised", shadow=0)
        expect(artwork.locator('[data-key="Q"] feDropShadow')).to_have_attribute("flood-opacity", "0")
        expect(artwork.locator('[data-key="Q"] g[transform="translate(0 3)"]')).to_have_count(1)

        # 使用合成渐变照片检查横向和纵向的连续裁切，0.2 与 0.3 不能落入同一裁切档位。
        for width, height in [(900, 290), (390, 900)]:
            photo = page.evaluate("""([width, height]) => {
                const canvas = document.createElement('canvas'); canvas.width = width; canvas.height = height;
                const context = canvas.getContext('2d');
                const gradient = context.createLinearGradient(0, 0, width > height ? width : 0, width > height ? 0 : height);
                gradient.addColorStop(0, 'red'); gradient.addColorStop(1, 'blue');
                context.fillStyle = gradient; context.fillRect(0, 0, width, height);
                return canvas.toDataURL('image/jpeg', 1).split(',')[1];
            }""", [width, height])
            samples = []
            for position in [0, .2, .3, 1]:
                render(photo=photo, photoPosition=position, photoShade=0)
                image = artwork.locator("foreignObject img")
                expect(image).to_have_js_property("complete", True)
                expect(image).to_have_css("object-position", f"50% {position * 100:g}%")
                samples.append(pixels(page, artwork, [(200, 280)])[0])
            if height > width:
                assert samples[0][0] > samples[1][0] > samples[2][0] > samples[3][0], samples
            else:
                assert samples[0] == samples[1] == samples[2] == samples[3], samples
            render(photo=photo, photoPosition=.3)
            shaded = pixels(page, artwork, [(200, 280)])[0]
            assert all(abs(actual - original * .75) < 3 for actual, original in zip(shaded, samples[2])), (shaded, samples[2])

        render(keyShape="capsule", keyMaterial="glass", pattern=3)
        form.get_by_role("radio", name="九键", exact=True).click()
        expect(form.get_by_role("img", name="新皮肤，九键键盘效果")).to_be_visible()
        expect(form.locator('[data-key="ABC"]')).to_have_count(1)
        editor.fill("{}")
        expect(form.get_by_role("status")).to_contain_text("暂不支持预览")
        editor.fill(json.dumps(design))
        expect(form.get_by_role("img", name="新皮肤，九键键盘效果")).to_be_visible()
        assert not errors, errors
        assert page.evaluate("window.cspViolations") == []
    finally:
        context.close()


def check_candidate_preview(browser, base):
    context = browser.new_context(viewport={"width": 1280, "height": 1000})
    page = context.new_page()
    errors, requests, preview_calls, detail_calls = [], [], [], []
    page.on("pageerror", lambda error: errors.append(str(error)))
    page.add_init_script("window.cspViolations = []; document.addEventListener('securitypolicyviolation', e => window.cspViolations.push(e.violatedDirective));")
    manifest = """schema_version = 1
id = 'synthetic-candidate'
name = '合成候选窗'
version = '1.0'
base = 'wechat'
preview = 'preview.png'
[supports]
layouts = ['horizontal', 'vertical']
themes = ['light', 'dark']
[candidate_window]
min_width_dip = 280
corner_radius_dip = 18
border_width_dip = 2
item_corner_radius_dip = 9
shadow = 'none'
page_arrows = true
[candidate_window.decoration]
image = 'assets/deco.png'
top_inset_dip = 80
width_dip = 70
align = 'left'
[candidate_window.background]
image = 'assets/bg.png'
fit = 'contain'
opacity = 0.4
[candidate.light]
surface = '#123456'
text = '#eeddcc'
selected = '#aabbcc'
selected_text = '#ffffff'
show_selected_bar = false
[candidate.dark]
surface = '#222333'
"""
    shell = smoke.FIXTURES["/api/shell"]
    item = {**smoke.FIXTURES["/api/skins"]["items"][0], "id": "candidate-one", "name": "合成候选窗", "edit_token": "revision-1"}
    detail = {**smoke.FIXTURES["/api/skins/smoke-skin"], "id": item["id"], "name": item["name"], "edit_token": item["edit_token"], "content": manifest}
    page.route("**/api/auth/session", lambda route: route.fulfill(json=smoke.FIXTURES["/api/auth/session"]))
    page.route("**/api/shell", lambda route: route.fulfill(json={**shell, "me": {**shell["me"], "permissions": ["review_community"]}}))
    page.route("**/api/candidate-skins?*", lambda route: route.fulfill(json={"items": [item], "page": 1, "total": 1, "has_more": False}))
    page.route("**/api/candidate-skins/candidate-one", lambda route: (detail_calls.append(route.request.url), route.fulfill(json=detail)))
    page.route("**/api/candidate-skins/candidate-one?view=preview", lambda route: (preview_calls.append(route.request.url), route.fulfill(json={"content": detail["content"]})))

    def image_response(route):
        path = parse_qs(urlparse(route.request.url).query)["path"][0]
        requests.append((detail["edit_token"], path))
        assert path in ("assets/deco.png", "assets/bg.png")
        route.fulfill(content_type="image/png", body=smoke.SMOKE_PNG)

    uploaded = []
    page.route("**/api/candidate-skins/*/image?*", image_response)
    page.route("**/api/candidate-skins/*/preview", lambda route: (uploaded.append(route.request.url), route.fulfill(content_type="image/png", body=smoke.SMOKE_PNG)))
    try:
        page.goto(base + "/community?tab=candidate-skins")
        card = page.get_by_role("button", name="查看「合成候选窗」详情", exact=True)
        expect(card.locator(".candidate-surface")).to_have_css("background-color", "rgb(18, 52, 86)")
        expect(card.get_by_alt_text("候选窗背景")).to_have_js_property("naturalWidth", 1)
        expect(card.get_by_alt_text("候选窗装饰")).to_have_js_property("naturalWidth", 1)
        assert not uploaded
        assert len(preview_calls) == 1 and not detail_calls
        page.get_by_role("button", name="合成候选窗", exact=True).click()
        drawer = page.get_by_role("dialog", name="合成候选窗", exact=True)
        expect(drawer.locator(".candidate-surface")).to_have_css("border-radius", "18px")
        assert len(detail_calls) == 1 and len(preview_calls) == 1
        expect(drawer.locator(".candidate-surface")).to_have_css("box-shadow", "none")
        expect(drawer.get_by_alt_text("候选窗背景")).to_have_css("object-fit", "contain")
        expect(drawer.get_by_alt_text("候选窗背景")).to_have_css("opacity", "0.4")
        expect(drawer.get_by_alt_text("候选窗装饰")).to_have_css("object-position", "0% 100%")
        expect(drawer.locator(".candidate-selected .candidate-word")).to_have_css("color", "rgb(255, 255, 255)")
        expect(drawer.locator(".candidate-selected")).to_have_css("background-color", "rgb(170, 187, 204)")
        drawer.get_by_role("radio", name="纵向", exact=True).click()
        expect(drawer.locator(".candidate-rows")).to_have_css("flex-direction", "column")
        drawer.get_by_role("radio", name="深色", exact=True).click()
        expect(drawer.locator(".candidate-surface")).to_have_css("background-color", "rgb(34, 35, 51)")
        assert len(requests) == 2, requests
        drawer.get_by_text("作者上传的预览图", exact=True).click()
        expect(drawer.get_by_role("img", name="合成候选窗上传的预览图")).to_have_js_property("naturalWidth", 1)
        assert len(uploaded) == 1

        drawer.get_by_role("button", name="编辑", exact=True).click()
        form = page.get_by_role("dialog", name="编辑候选窗皮肤", exact=True)
        editor = form.get_by_label("皮肤清单（skin.toml）", exact=True)
        expect(form.locator(".candidate-surface")).to_have_css("background-color", "rgb(18, 52, 86)")
        editor.fill(manifest.replace("#123456", "#abcdef"))
        expect(form.locator(".candidate-surface")).to_have_css("background-color", "rgb(171, 205, 239)")
        # 替换图片只使用待保存文件，绝不混入旧版远程背景或作者的截图。
        form.get_by_label("替换全部图片（可选）", exact=True).set_input_files([
            {"name": "deco.png", "mimeType": "image/png", "buffer": smoke.SMOKE_PNG},
            {"name": "bg.png", "mimeType": "image/png", "buffer": smoke.SMOKE_PNG},
        ])
        form.get_by_label("deco.png 的包内路径", exact=True).fill("assets/deco.png")
        form.get_by_label("bg.png 的包内路径", exact=True).fill("assets/bg.png")
        expect(form.get_by_alt_text("候选窗背景")).to_have_js_property("naturalWidth", 1)
        expect(form.get_by_alt_text("候选窗装饰")).to_have_js_property("naturalWidth", 1)
        assert len(requests) == 2, requests
        editor.fill(manifest.replace("assets/bg.png", "https://example.test/bg.png"))
        expect(form.get_by_role("status")).to_contain_text("背景或装饰图片缺失")
        expect(form.get_by_alt_text("候选窗背景")).to_have_count(0)
        editor.fill("invalid = [")
        expect(form.get_by_role("status")).to_contain_text("请填写有效清单")
        imported = manifest.replace("#123456", "#654321").replace("layouts = ['horizontal', 'vertical']", "layouts = ['vertical']").replace("themes = ['light', 'dark']", "themes = ['dark']")
        form.get_by_label("导入 skin.toml（可选）", exact=True).set_input_files({"name": "skin.toml", "mimeType": "text/plain", "buffer": imported.encode()})
        expect(form.locator(".candidate-art")).to_have_attribute("data-layout", "vertical")
        expect(form.locator(".candidate-art")).to_have_attribute("data-mode", "dark")
        expect(form.locator(".candidate-surface")).to_have_css("background-color", "rgb(34, 35, 51)")
        form.get_by_role("button", name="取消", exact=True).click()
        page.keyboard.press("Escape")

        # 同一个页面里刷新到新版本，仍用相同资源路径时也必须重新读取图片。
        detail["edit_token"] = item["edit_token"] = "revision-2"
        detail["content"] = manifest.replace("#123456", "#654321")
        page.get_by_role("radio", name="待审核", exact=True).click()
        expect(card.locator(".candidate-surface")).to_have_css("background-color", "rgb(101, 67, 33)")
        expect(card.get_by_alt_text("候选窗背景")).to_have_js_property("naturalWidth", 1)
        assert len(requests) == 4, requests
        assert len(preview_calls) == 2 and len(detail_calls) == 1
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
            check_skin_preview(browser, f"http://127.0.0.1:{server.server_port}")
            check_candidate_preview(browser, f"http://127.0.0.1:{server.server_port}")
            browser.close()
        print("皮肤预览浏览器验证通过")
    finally:
        server.shutdown()
