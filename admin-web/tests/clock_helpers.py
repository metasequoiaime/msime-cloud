"""在可控时钟测试中等待真实网络与页面通知完成。"""

import datetime

from playwright.sync_api import expect


def expect_clocked_visible(page, locator):
    # 网络响应和 Query 通知不与虚拟时间同步；计时边界断言结束后恢复时钟，等待页面实际更新。
    page.clock.resume()
    expect(locator).to_be_visible()
    # 再次暂停前推进一秒虚拟时间，确保暂停点在当前时间之后；此时尚未开始下一段输入计时。
    pause_at = datetime.datetime.fromtimestamp((page.evaluate("Date.now()") + 1000) / 1000, datetime.timezone.utc)
    page.clock.pause_at(pause_at)
