#!/usr/bin/env python3
"""CSP smoke test for the admin console build.

Serves admin-web/dist with the exact Content-Security-Policy that internal/server/admin.go sends, answers the shell's /api calls with fixed test fixtures, drives every route and the shell popovers in headless Chromium, and fails on any CSP violation, page error or missing shell element.

It also builds tests/harness (every shared ui component with test fixtures) into a temporary directory and exercises the data table, confirm dialog, toast, drawer and charts the same way.

Usage: python3 admin-web/tests/csp_smoke.py   (after `pnpm build`; needs pnpm and `pip install playwright && playwright install chromium`)
"""

import json
import re
import subprocess
import sys
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

from playwright.sync_api import TimeoutError as PlaywrightTimeout
from playwright.sync_api import expect, sync_playwright

ROOT = Path(__file__).resolve().parents[2]
DIST = ROOT / "admin-web" / "dist"
ADMIN_GO = ROOT / "internal" / "server" / "admin.go"

PAGES = {
    "/": "数据概览", "/dictpr": "词库审核", "/community": "社区审核", "/issues": "问题分诊", "/feedback": "用户反馈", "/words": "敏感词库",
    "/users": "用户账号", "/downloads": "下载记录", "/notice": "公告推送", "/release": "发布管理", "/cloud": "云端监控",
    "/crash": "崩溃上报", "/status": "系统状态", "/logs": "服务日志", "/perm": "权限日志", "/me": "个人中心",
}
REDIRECTS = {"/admins": "/perm", "/audit": "/perm", "/system": "/status", "/crashes": "/crash", "/skins": "/community?tab=skins", "/dictionaries": "/community?tab=dictionaries", "/replies": "/community?tab=replies", "/site-settings": "/downloads"}

# Test fixtures for the shell endpoints, shaped like gap.md section 1.
FIXTURES = {
    "/api/auth/session": {"version": "0.34.0", "authenticated": True, "email": "owner@example.com", "google_enabled": True, "token_enabled": False, "can_manage_admins": True},
    "/api/shell": {"version": "0.34.0", "environment": "测试环境", "me": {"email": "owner@example.com", "name": "Owner", "role": "maintainer", "permissions": ["review_dict_pr", "manage_permissions"]}, "pending": {"dict_prs": 2, "community": 0, "issues": 5}, "unread_notifications": 1, "status": "degraded"},
    "/api/notifications": {"items": [{"id": 1, "kind": "dict_pr", "title": "词库 PR #9 等待审核", "target_page": "dictpr", "target_id": "9", "created_at": "2026-10-01T00:00:00Z", "read": False}], "unread": 1},
    "/api/notifications/read": {"ok": True},
    "/api/search": {"items": [{"kind": "user", "id": "u1", "title": "smoke-user", "where": "用户账号", "target": "users"}]},
    "/api/auth/logout": {"ok": True},
    "/api/dict-prs": {"repo": "metasequoiaime/msime-dictionary", "counts": {"open": 1, "merged": 1, "closed": 0, "all": 2}, "items": [
        {"number": 9, "title": "feat(custom): add 2 words", "author": "msime-words[bot]", "author_bot": True, "created_at": "2026-10-01T00:00:00Z", "updated_at": "2026-10-01T00:00:00Z", "state": "open", "url": "https://github.com/metasequoiaime/msime-dictionary/pull/9", "note": "地名", "counts": {"total": 2, "new": 1, "dup": 1, "flagged": 0}},
        {"number": 8, "title": "feat(custom): add 1 word", "author": "msime-words[bot]", "author_bot": True, "created_at": "2026-09-20T00:00:00Z", "updated_at": "2026-09-21T00:00:00Z", "state": "merged", "url": "https://github.com/metasequoiaime/msime-dictionary/pull/8", "note": "", "counts": None},
    ]},
    "/api/dict-prs/9": {"repo": "metasequoiaime/msime-dictionary", "head_sha": "head9", "mergeable": True, "submissions": [{"kind": "words", "note": "地名", "created_at": "2026-10-01T00:00:00Z"}],
        "pull": {"number": 9, "title": "feat(custom): add 2 words", "author": "msime-words[bot]", "author_bot": True, "created_at": "2026-10-01T00:00:00Z", "updated_at": "2026-10-01T00:00:00Z", "state": "open", "url": "https://github.com/metasequoiaime/msime-dictionary/pull/9", "note": "地名", "counts": {"total": 2, "new": 1, "dup": 1, "flagged": 0}},
        "entries": [
            {"index": 0, "file": "custom/words.txt", "kind": "words", "word": "江汉油田", "pinyin": "jiang'han'you'tian", "flag": "new"},
            {"index": 1, "file": "custom/words.txt", "kind": "words", "word": "潜江", "pinyin": "qian'jiang", "flag": "dup", "reason": "词库中已有这个词条"},
        ]},
    "/api/dict-prs/9/trim": {"ok": True, "count": 1, "removed": 1, "head_sha": "head10"},
    "/api/dict-prs/9/approve": {"ok": True, "merged": True, "count": 1, "removed": 1},
    "/api/sensitive-words": {"items": [
        {"id": 2, "pattern": "(微信|vx)[\\s:：]*[a-z0-9_-]{5,}", "is_regex": True, "category": "ad", "level": "block", "created_by": "owner@example.com", "created_at": "2026-09-02T08:00:00Z", "hits_7d": 41},
        {"id": 1, "pattern": "代购", "is_regex": False, "category": "custom", "level": "review", "created_by": "legacy-token", "created_at": "2026-08-20T08:00:00Z", "hits_7d": 0},
    ], "max_hits": 41},
    # Users page (U5): stats tiles, one list page and the detail the ?focus=u1 search result opens.
    "/api/users/stats": {"total": 2, "new_7d": 1, "sync_ratio": 0.5, "banned": 1, "roles": {"maintainer": 1, "user": 1}},
    "/api/users": {"items": [
        {"id": "u1", "display_name": "smoke-user", "created_at": "2026-09-01T00:00:00Z", "sessions": 1, "devices": 1, "last_active": "2026-10-01T00:00:00Z", "contact": "s***@example.com", "contact_kind": "email", "role": "maintainer", "banned": False, "banned_at": None, "ban_reason": ""},
        {"id": "u2", "display_name": "", "created_at": "2026-09-02T00:00:00Z", "sessions": 0, "devices": 0, "last_active": None, "contact": "+86****2201", "contact_kind": "phone", "role": "user", "banned": True, "banned_at": "2026-09-30T00:00:00Z", "ban_reason": "发布广告导流"},
    ], "page": 1, "total": 2, "has_more": False},
    "/api/users/u1": {"id": "u1", "display_name": "smoke-user", "created_at": "2026-09-01T00:00:00Z", "providers": ["email"], "role": "maintainer", "contact": "s***@example.com", "contact_kind": "email",
                      "banned": False, "banned_at": None, "ban_reason": "", "banned_by": "", "sync": True, "last_active": "2026-10-01T00:00:00Z", "active_sessions": 1, "total_sessions": 1, "skins": 1, "candidate_skins": 0, "plugins": 0, "dictionaries": 0, "replies": 0,
                      "sessions": [{"id": "s1", "created_at": "2026-09-30T00:00:00Z", "expires_at": "2026-10-30T00:00:00Z", "last_active": "2026-10-01T00:00:00Z", "user_agent": "MSIME/0.5.4 (Windows 11)", "status": "active"}],
                      "works": [{"section": "skins", "id": "k1", "name": "水杉秋色", "moderation": "approved", "moderation_reason": "", "created_at": "2026-09-15T00:00:00Z", "downloads": 3880, "saves": 0}],
                      "history": [{"id": 1, "action": "revoke_session", "actor": "owner@example.com", "detail": {}, "created_at": "2026-09-20T00:00:00Z"}]},
    "/api/downloads/summary": {"day": "2026-10-01", "rows": [
        {"source": "github_release", "platform": "windows", "version": "v0.5.4", "artifact": "msime-windows-x64-setup.exe", "channel": "github", "repo": "metasequoiaime/msime-windows", "tag": "windows-v0.5.4", "today": 812, "week": 5120},
        {"source": "telemetry", "platform": "windows", "version": "v0.5.4", "artifact": "x64 安装包", "channel": "cn-mirror", "today": 388, "week": 2410},
        {"source": "telemetry", "platform": "android", "version": "0.1.0", "artifact": None, "channel": None, "today": 0, "week": 3},
    ], "totals": {"today": 1200, "week": 7533, "github_today": 812, "github_week": 5120, "mirror_week": 2410}, "mirror_share": 0.32, "channel_reported": True, "snapshot_day": "2026-10-01", "truncated": False},
    "/api/site-settings": {"lanzou_url": "https://wwbn.lanzouq.com/iAbc123", "updated_at": "2026-09-30T08:00:00Z", "updated_by": "google:1:owner@example.com"},
    "/api/notices": {"telegram": False, "items": [
        {"id": "2", "title": "词库共建上线：打不出来的词直接提交", "body": "官网新增词条提交入口。", "targets": ["all"], "channels": ["site"], "status": "live", "created_by": "google:1:owner@example.com", "author": "owner@example.com", "created_at": "2026-09-27T01:30:00Z", "published_at": "2026-09-27T01:30:00Z", "updated_at": "2026-09-27T01:30:00Z"},
        {"id": "3", "title": "Windows 10 工具栏图标方框的临时处理办法", "body": "", "targets": ["windows"], "channels": ["app"], "status": "draft", "created_by": "legacy-token", "author": "legacy-token", "created_at": "2026-09-30T08:00:00Z", "published_at": None, "updated_at": "2026-09-30T08:00:00Z"},
    ]},
    # U10 cloud and status pages, shaped like internal/server/admin_cloud.go and admin_status.go.
    "/api/cloud": {"generated_at": "2026-10-01T00:00:00Z", "since": "2026-09-30T01:00:00Z", "month_start": "2026-10-01T00:00:00Z", "services": [
        {"key": "chat", "name": "AI 联想", "provider": "账号通道", "state": "degraded", "calls_24h": 38204, "errors_24h": 497, "error_rate": 0.013, "p95_ms": 1840, "slow_ms": 3000,
         "hourly": [{"hour": f"2026-09-30T{h:02d}:00:00Z", "calls": 1000 + h * 40, "errors": h} for h in range(24)],
         "month": {"calls": 24800, "errors": 300, "usage": 0, "meter": "calls"}, "quota": {"limit": 2000, "unit": "cny", "used": 1240, "pct": 62}, "cost_cny": 1240},
        {"key": "translation", "name": "在线翻译", "provider": "腾讯 TMT", "state": "idle", "calls_24h": 0, "errors_24h": 0, "error_rate": None, "p95_ms": None, "slow_ms": 3000,
         "hourly": [{"hour": f"2026-09-30T{h:02d}:00:00Z", "calls": 0, "errors": 0} for h in range(24)],
         "month": {"calls": 0, "errors": 0, "usage": 0, "meter": "chars"}, "quota": None, "cost_cny": None},
    ]},
    "/api/status": {"checked_at": "2026-10-01T00:00:00Z", "state": "degraded", "services": [
        {"key": "database", "name": "数据库", "desc": "PostgreSQL", "state": "ok", "p95_ms": 3, "uptime_60d": 1,
         "days": [{"day": f"d{i}", "state": "ok" if i > 30 else "none", "uptime": 1 if i > 30 else None} for i in range(60)]},
        {"key": "chat", "name": "AI 联想", "desc": "账号通道", "state": "degraded", "p95_ms": 1800, "uptime_60d": 0.987,
         "days": [{"day": f"d{i}", "state": "down" if i == 12 else "degraded" if i == 59 else "ok", "uptime": 0.98} for i in range(60)]},
    ], "incidents": [
        {"id": 7, "service": "chat", "service_name": "AI 联想", "title": "AI 联想响应变慢", "description": "最近 5 分钟 40 次调用，失败 0 次（0.0%），P95 4.2s，阈值 3.0s。", "state": "open", "started_at": "2026-10-01T00:00:00Z", "resolved_at": None, "auto": True},
        {"id": 6, "service": "cloud", "service_name": "云候选", "title": "云候选间歇超时", "description": "", "state": "resolved", "started_at": "2026-09-03T01:00:00Z", "resolved_at": "2026-09-03T01:38:00Z", "auto": False},
    ]},
    # GET /api/overview (unit U11): real response shape with client telemetry present for some platforms.
    "/api/overview": {
        "users": 12, "new_users_30d": 3, "session_users": 4, "downloads": 4821, "crashes": 9, "open_crashes": 2, "skins": 5, "skin_downloads": 40, "plugins": 1, "plugin_downloads": 3,
        "dictionaries": 2, "replies": 1, "resource_saves": 6, "range_days": 30,
        "daily": [{"day": f"2026-09-{d:02d}", "users": 0, "downloads": d, "crashes": 0} for d in range(1, 31)],
        "downloads_30d": 410, "downloads_prev_30d": 380,
        "telemetry": {"active": True, "sessions": True},
        "active_devices_daily": [{"day": f"2026-09-{d:02d}", "windows": 50 + d, "mac_linux": 20 + d % 5, "mobile": 10 + d % 7} for d in range(1, 31)],
        "active_devices_7d": 131, "active_devices_prev_7d": 120,
        "platform_active_7d": {"windows": 80, "macos": 21, "ios": 18, "harmonyos": 12},
        "crash_free_rate": 0.9962, "crash_free_rate_prev": 0.997,
        "crash_top": {"platform": "ios", "version": "1.0.0", "crashes": 3},
        "crash_group_latest": {"signature": "0123456789abcdef", "platform": "ios", "title": "内存超限"},
        "pending": {"community": 0, "reports_7d": 2, "crash_groups": 1},
        "services_configured": True,
        "services": [
            {"key": "cloud", "name": "云候选代理", "provider": "腾讯云", "state": "ok", "uptime_60d": 99.95, "p95_ms": 120},
            {"key": "chat", "name": "AI 联想", "provider": "OpenAI", "state": "degraded", "uptime_60d": 98.7, "p95_ms": 1800},
            {"key": "translation", "name": "翻译（账号通道）", "provider": "", "state": "unknown", "uptime_60d": None, "p95_ms": None},
        ],
    },
    # Personal page and permissions (U12), shaped like internal/account/admin_me.go and admin_permissions.go.
    "/api/me": {
        "email": "owner@example.com", "name": "Owner", "role": "maintainer", "owner": True, "via": "session", "joined_at": "2024-03-01T00:00:00Z",
        "stats": {"dict_prs_month": 38, "community_month": 112, "issues_month": 54, "avg_handle_hours": 3.1},
        "prefs": {"notify_dict_pr": True, "notify_report": True, "notify_crash_spike": False, "weekly_digest": False},
        "recent": [{"id": 2, "action": "dict_pr_approve", "target": "210", "detail": {"count": 5}, "created_at": "2026-10-01T00:00:00Z"}],
        "sessions": [{"id": "0123456789abcdef", "created_at": "2026-10-01T00:00:00Z", "last_seen_at": "2026-10-01T00:00:00Z", "device": "macOS · Safari", "current": True},
                     {"id": "fedcba9876543210", "created_at": "2026-09-30T00:00:00Z", "last_seen_at": "2026-09-30T08:00:00Z", "device": "Windows · Edge", "current": False}],
        "token": {"last4": "7c2e", "created_at": "2026-10-01T00:00:00Z", "expires_at": "2026-10-31T00:00:00Z"},
    },
    "/api/permissions": {
        "roles": [{"key": "maintainer", "name": "维护者", "builtin": True}, {"key": "reviewer", "name": "审核志愿者", "builtin": True}, {"key": "operator", "name": "运营/客服", "builtin": True}, {"key": "readonly", "name": "只读", "builtin": True}],
        "permissions": ["review_dict_pr", "review_community", "triage_issues", "ban_users", "publish_notices", "trigger_release", "view_cloud_usage", "view_logs", "manage_permissions"],
        "matrix": {"maintainer": ["review_dict_pr", "review_community", "triage_issues", "ban_users", "publish_notices", "trigger_release", "view_cloud_usage", "view_logs", "manage_permissions"], "reviewer": ["review_dict_pr", "review_community", "triage_issues"], "operator": ["triage_issues", "ban_users", "publish_notices", "view_cloud_usage"], "readonly": ["view_cloud_usage"]},
        "members": [{"email": "owner@example.com", "role": "maintainer", "enabled": True, "owner": True, "sessions": 1, "last_seen_at": "2026-10-01T00:00:00Z", "created_at": None},
                    {"email": "helper@example.com", "role": "reviewer", "enabled": False, "owner": False, "sessions": 0, "last_seen_at": None, "created_at": "2026-09-01T00:00:00Z"}],
    },
    "/api/audit": {"items": [{"id": 3, "actor": "google:1:owner@example.com", "action": "permission_revoke", "target": "operator", "detail": {"role": "operator", "role_name": "运营/客服", "permission": "ban_users"}, "created_at": "2026-10-01T00:00:00Z"}], "page": 1, "total": 1, "has_more": False},
}

# Issue triage (U3): one open issue on the list and its detail, shaped like internal/server/admin_issues.go.
SMOKE_ISSUE = {"repo": "metasequoiaime/msime", "number": 12, "title": "Shift 切换偶尔失效", "author": "smoke-author", "url": "https://github.com/metasequoiaime/msime/issues/12", "state": "new", "platform": "linux", "kind": "bug", "labels": ["bug", "linux", "快捷键"], "assignees": [], "comments": 1, "created_at": "2026-10-01T00:00:00Z", "updated_at": "2026-10-01T00:00:00Z", "closed_at": None}
FIXTURES.update({
    "/api/issues": {"items": [SMOKE_ISSUE], "page": 1, "page_size": 50, "total": 1, "has_more": False, "repos": ["metasequoiaime/msime"], "platforms": [{"id": "linux", "name": "Linux", "label": "linux", "assignee": "houko"}], "unavailable": [], "platform_counts": {"all": 1, "other": 0, "linux": 1}, "state_counts": {"new": 1, "triaged": 0, "done": 0, "dup": 0}, "stats": {"pending": 1, "triaged": 0, "new_this_week": 1, "first_response_hours": 2.5, "first_response_samples": 1}},
    "/api/issues/metasequoiaime/msime/12": {"issue": {**SMOKE_ISSUE, "body": "连按 Shift 偶尔没有反应。"}, "timeline": [{"kind": "created", "actor": "smoke-author", "text": "", "at": "2026-10-01T00:00:00Z"}, {"kind": "commented", "actor": "houko", "text": "需要日志", "at": "2026-10-01T01:00:00Z"}], "timeline_truncated": False, "similar": [], "platform_assignee": "houko"},
})


# U2 community moderation: counts, the skin list and one skin detail, so the smoke renders real cards, the mini keyboard and the drawer's keyboard preview.
SMOKE_DESIGN = {"background": 15266027, "keyBackground": 16777215, "keyForeground": 1516829, "accent": 1596487, "actionBackground": 1596487, "cornerRadius": 8, "borderWidth": 0, "shadow": 0, "pattern": 0, "monospaced": False, "gradientEnd": 16304344}
SMOKE_STATES = {"pending": 1, "approved": 0, "removed": 0}
FIXTURES.update({
    "/api/community/counts": {"skins": SMOKE_STATES, "candidate-skins": SMOKE_STATES, "plugins": {**SMOKE_STATES, "pending": 0}, "dictionaries": SMOKE_STATES, "replies": SMOKE_STATES, "phrases": SMOKE_STATES},
    "/api/skins": {"items": [{"id": "smoke-skin", "name": "春日樱", "description": "粉色", "owner_id": "u1", "author": "smoke-author", "created_at": "2026-10-01T00:00:00Z", "design": SMOKE_DESIGN, "downloads": 12, "moderation": "pending", "moderation_reason": "命中敏感词：「加V」", "moderated_by": None, "moderated_at": None, "flag": "命中敏感词：「加V」", "reports": 1}], "page": 1, "total": 1, "has_more": False},
    "/api/skins/smoke-skin": {"id": "smoke-skin", "name": "春日樱", "description": "粉色", "owner_id": "u1", "author": "smoke-author", "created_at": "2026-10-01T00:00:00Z", "content": SMOKE_DESIGN, "moderation": "pending", "previous_moderation": None, "moderation_reason": None, "moderated_by": None, "moderated_at": None, "owner_banned": False, "downloads": 12, "rating_count": 0, "rating_average": 0, "reports": [{"id": 1, "reason": "商标侵权", "detail": "附截图", "reporter": "smoke-reader", "created_at": "2026-10-01T00:00:00Z"}], "report_count": 1, "flags": [], "owner_items": [{"section": "replies", "id": "r1", "name": "委婉拒绝", "moderation": "approved", "created_at": "2026-09-01T00:00:00Z"}]},
})

# Release page fixtures (U7), shaped like internal/server/admin_releases.go.
RELEASE_PLATFORM = {"id": "windows", "name": "Windows", "repo": "metasequoiaime/msime-windows", "tag_prefix": "windows-v", "workflow": "release.yml"}
RELEASE_CURRENT = {"id": 3, "tag": "windows-v0.5.4", "version": "v0.5.4", "name": "", "status": "released", "created_at": "2026-09-25T08:00:00Z", "published_at": "2026-09-26T08:00:00Z", "author": "houko", "body": "### 新增\n- 剪贴板历史支持固定条目", "notes": [{"kind": "新增", "text": "剪贴板历史支持固定条目"}], "assets": [{"name": "msime-windows-x64-setup.exe", "size": 19084083, "downloads": 120, "url": "https://example.test/x64"}], "downloads": 120, "url": "https://example.test/release"}
RELEASE_WITHDRAWN = {**RELEASE_CURRENT, "id": 1, "tag": "windows-v0.5.3", "version": "v0.5.3", "status": "withdrawn", "body": "### 说明\n- 缺少 vc_redist", "notes": [{"kind": "说明", "text": "缺少 vc_redist"}], "downloads": 7, "assets": []}
FIXTURES.update({
    "/api/releases": {"platforms": [
        {**RELEASE_PLATFORM, "latest": RELEASE_CURRENT, "checklist": [{"key": "ci", "label": "CI 全部通过", "state": "passed", "note": ""}, {"key": "store", "label": "商店 / 分发渠道", "state": "passed", "note": "GitHub Release 已公开"}]},
        {"id": "ios", "name": "iOS", "repo": "metasequoiaime/msime", "tag_prefix": "ios-v", "workflow": "", "latest": None, "checklist": [], "error": "github_unavailable"},
    ]},
    "/api/releases/windows": {"platform": RELEASE_PLATFORM, "releases": [RELEASE_CURRENT, RELEASE_WITHDRAWN]},
})

# Crash page (U9): one open group without install ids and one known group with a GitHub issue and a stack sample.
CRASH_GROUP = {"signature": "0123456789abcdef", "platform": "ios", "version": "1.0.0", "title": "EXC_BAD_ACCESS", "status": "known", "issue_url": "https://github.com/metasequoiaime/msime-ios/issues/7", "first_seen": "2026-09-20T00:00:00Z", "last_seen": "2026-10-01T00:00:00Z", "count_7d": 12, "count_prev_7d": 9, "devices_7d": 8, "new": False}
FIXTURES.update({
    "/api/feedback": {"items": [{"id": "0b6f2c1e-5d3a-4c8e-9f10-2a7b8c9d0e1f", "type": "bug", "text": "候选栏偶尔不显示\n重启后恢复", "platform": "android", "app_version": "2.1.0", "edition": "pinyin", "diagnostics": {"device": "Pixel 8"}, "status": "new", "created_at": "2026-10-01T00:00:00Z", "user_id": "u1", "author": "smoke-user", "anonymous": False, "screenshots": 0}], "page": 1, "total": 1, "has_more": False},
    "/api/crash-groups": {"items": [CRASH_GROUP, {**CRASH_GROUP, "signature": "fedcba9876543210", "platform": "windows", "title": "Server exited", "status": "open", "issue_url": None, "count_7d": 3, "count_prev_7d": 0, "devices_7d": None, "new": True}], "has_more": False, "platforms": [{"platform": "ios", "count": 1}, {"platform": "windows", "count": 1}], "summary": {"groups": 2, "crashes_7d": 15, "devices_7d": 8, "installs_today": None, "crash_free_rate": 0.9962}},
    "/api/crash-groups/0123456789abcdef": {"group": CRASH_GROUP, "samples": [{"id": "crash-1", "platform": "ios", "version": "1.0.0", "message": "EXC_BAD_ACCESS", "stack": "2 MSIME 0x1 KeyboardViewController.layoutCandidates() + 120", "resolved": False, "created_at": "2026-10-01T00:00:00Z"}]},
    "/api/crash-groups/0123456789abcdef/issue": {"target": {"platform": "ios", "name": "iOS", "repo": "metasequoiaime/msime-ios"}, "issue_url": CRASH_GROUP["issue_url"]},
    "/api/crash-groups/fedcba9876543210/issue": {"target": None, "issue_url": None},
})


# 服务日志页的日志流，形如 internal/server/admin_logs.go 的 Server-Sent Events。首次连接回填两行并发送副本列表；带 cursor 的重连没有新行。
LOG_PODS = ["app-msime-backend-77f98747f9-4nlh4", "app-msime-backend-77f98747f9-hkxbc"]
LOG_LINES = [
    {"ts": "1790866019178204841", "time": "2026-10-01T14:46:54.755845624Z", "pod": LOG_PODS[1], "stream": "stderr", "level": "INFO", "message": "MSIME service listening address=0.0.0.0:8080"},
    {"ts": "1790866021452052720", "time": "2026-10-01T14:47:01.378828677Z", "pod": LOG_PODS[0], "stream": "stderr", "level": "WARN", "message": "http request method=GET route=\"GET /v1/cloud/candidates\" status=502 duration_ms=31 bytes=60"},
]


def log_stream(query: str) -> bytes:
    frames = ["retry: 5000\n\n"]
    if "cursor=" not in query:
        frames.append(f"id: {LOG_LINES[-1]['ts']}\nevent: lines\ndata: {json.dumps({'lines': LOG_LINES})}\n\n")
        frames.append(f"event: pods\ndata: {json.dumps({'pods': LOG_PODS})}\n\n")
    frames.append(": ping\n\n")
    return "".join(frames).encode()


def production_csp() -> str:
    match = re.search(r'Header\(\)\.Set\("Content-Security-Policy", "([^"]+)"\)', ADMIN_GO.read_text())
    if not match:
        raise SystemExit(f"could not find the admin CSP in {ADMIN_GO}")
    return match.group(1)


class Handler(BaseHTTPRequestHandler):
    csp = ""
    harness = Path()
    logged_out = False
    # posts records the path of every POST in arrival order.
    posts: list[str] = []

    def log_message(self, *_args):
        pass

    def send(self, status: int, body: bytes, content_type: str):
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Content-Security-Policy", self.csp)
        self.send_header("Cache-Control", "no-store")
        self.send_header("X-Content-Type-Options", "nosniff")
        self.end_headers()
        self.wfile.write(body)

    def api(self):
        path, _, query = self.path.partition("?")
        if path == "/api/logs/stream":
            return self.send(200, log_stream(query), "text/event-stream; charset=utf-8")
        if path == "/api/auth/logout":
            Handler.logged_out = True
        body = FIXTURES.get(path)
        if path == "/api/me" and self.command == "POST":
            body = {"ok": True}
        if path == "/api/auth/session" and Handler.logged_out:
            body = {**FIXTURES[path], "authenticated": False, "email": ""}
        if body is None:
            return self.send(404, json.dumps({"error": {"code": "not_found", "message": "not_found"}}).encode(), "application/json")
        return self.send(200, json.dumps(body).encode(), "application/json")

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        self.rfile.read(length)
        Handler.posts.append(self.path.split("?", 1)[0])
        if self.path.startswith("/api/"):
            return self.api()
        return self.send(405, b"", "text/plain")

    def do_GET(self):
        if self.path.startswith("/api/"):
            return self.api()
        path = self.path.split("?", 1)[0]
        if path == "/harness/":
            return self.send(200, (self.harness / "index.html").read_bytes(), "text/html; charset=utf-8")
        if path.startswith("/assets/") or path.startswith("/harness/assets/"):
            file = (self.harness / path.removeprefix("/harness/")) if path.startswith("/harness/") else DIST / path.lstrip("/")
            if not file.is_file():
                return self.send(404, b"", "text/plain")
            types = {".js": "text/javascript", ".css": "text/css", ".png": "image/png", ".woff2": "font/woff2", ".woff": "font/woff"}
            return self.send(200, file.read_bytes(), types.get(file.suffix, "application/octet-stream"))
        if path in PAGES or path in REDIRECTS:
            return self.send(200, (DIST / "index.html").read_bytes(), "text/html; charset=utf-8")
        return self.send(404, b"", "text/plain")


class Server(ThreadingHTTPServer):
    # The page preloads dozens of chunks at once; the default listen backlog of 5 makes macOS reset the overflow connections, which leaves the app blank.
    request_queue_size = 128

    # The browser aborts superseded requests (prefetches, cancelled queries); a closed socket is not a test failure.
    def handle_error(self, request, client_address):
        if not isinstance(sys.exc_info()[1], (BrokenPipeError, ConnectionResetError)):
            super().handle_error(request, client_address)


def main() -> int:
    if not (DIST / "index.html").is_file():
        raise SystemExit("admin-web/dist is missing; run pnpm build first")
    Handler.csp = production_csp()
    harness_dir = tempfile.TemporaryDirectory(prefix="msime-admin-harness-")
    subprocess.run(["pnpm", "exec", "vite", "build", "--logLevel", "warn", "--config", "tests/harness/vite.config.ts", "--outDir", harness_dir.name], cwd=ROOT / "admin-web", check=True)
    Handler.harness = Path(harness_dir.name)
    server = Server(("127.0.0.1", 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    base = f"http://127.0.0.1:{server.server_address[1]}"
    problems: list[str] = []
    try:
        with sync_playwright() as playwright:
            browser = playwright.chromium.launch()
            try:
                context = browser.new_context(viewport={"width": 1280, "height": 860})
                context.add_init_script("window.__csp = []; document.addEventListener('securitypolicyviolation', e => window.__csp.push(e.violatedDirective + ' ' + (e.blockedURI || 'inline') + ' ' + (e.sourceFile || '') + ':' + e.lineNumber));")
                page = context.new_page()
                page.on("pageerror", lambda error: problems.append(f"page error: {error}"))
                page.on("console", lambda message: problems.append(f"console {message.type}: {message.text}") if message.type == "error" and "Failed to load resource" not in message.text else None)

                def violations(label: str):
                    found = page.evaluate("window.__csp.splice(0)")
                    problems.extend(f"CSP violation on {label}: {item}" for item in found)

                for path, title in PAGES.items():
                    page.goto(base + path)
                    expect(page.get_by_role("complementary", name="后台导航")).to_be_visible()
                    expect(page.locator("header h1")).to_have_text(title)
                    violations(path)

                page.goto(base + "/downloads")
                expect(page.get_by_role("table", name="下载记录")).to_contain_text("官网镜像（国内）")
                expect(page.get_by_text("32%", exact=True)).to_be_visible()
                page.get_by_role("radio", name=re.compile("^Android")).click()
                page.wait_for_url(re.compile(r"/downloads\?platform=android$"))
                expect(page.get_by_role("table", name="下载记录")).not_to_contain_text("GitHub Release")
                page.goto(base + "/downloads?platform=bogus")
                expect(page.get_by_role("table", name="下载记录")).to_contain_text("GitHub Release")
                violations("downloads")

                page.goto(base + "/crash")
                expect(page.get_by_text("99.62%")).to_be_visible()
                expect(page.get_by_role("button", name="建 Issue")).to_be_disabled()
                page.get_by_role("button", name="查看堆栈").click()
                drawer = page.get_by_role("dialog", name="EXC_BAD_ACCESS")
                expect(drawer.get_by_text("KeyboardViewController.layoutCandidates() + 120")).to_be_visible()
                expect(drawer.get_by_text("metasequoiaime/msime-ios", exact=True)).to_be_visible()
                page.keyboard.press("Escape")
                expect(drawer).to_be_hidden()
                violations("crash drawer")

                page.goto(base + "/cloud")
                expect(page.get_by_text("62% · ¥ 1,240 / 2,000")).to_be_visible()
                expect(page.get_by_text("近 24 小时无调用")).to_be_visible()
                violations("/cloud data")
                page.goto(base + "/status?focus=7")
                expect(page.get_by_role("img", name="AI 联想近 60 天可用性")).to_be_visible()
                expect(page.get_by_text("1 项服务正常，AI 联想响应变慢或出错")).to_be_visible()
                expect(page.get_by_text("云候选间歇超时")).to_be_visible()
                violations("/status data")

                # 服务日志：功能已启用且角色有 view_logs 时，页面连上日志流，显示两个副本的行和副本按钮，可以暂停。
                shell_fixture = FIXTURES["/api/shell"]
                FIXTURES["/api/shell"] = {**shell_fixture, "features": {"logs": True}, "me": {**shell_fixture["me"], "permissions": [*shell_fixture["me"]["permissions"], "view_logs"]}}
                page.goto(base + "/logs")
                log = page.get_by_role("log", name="服务日志")
                expect(log).to_contain_text("MSIME service listening")
                expect(log).to_contain_text("GET /v1/cloud/candidates")
                expect(page.get_by_role("radio", name="77f98747f9-hkxbc")).to_be_visible()
                expect(page.get_by_role("complementary", name="后台导航").get_by_role("link", name="服务日志")).to_be_visible()
                page.get_by_role("button", name="暂停").click()
                expect(page.get_by_text("已暂停")).to_be_visible()
                page.get_by_role("button", name="继续").click()
                violations("/logs data")
                FIXTURES["/api/shell"] = shell_fixture

                for old, new in REDIRECTS.items():
                    page.goto(base + old)
                    page.wait_for_url(base + new)
                    violations(old)

                page.goto(base + "/dictpr")
                expect(page.get_by_role("list", name="#9 的词条")).to_contain_text("江汉油田")
                expect(page.get_by_text("已勾选 1 / 2 条", exact=False)).to_be_visible()
                page.get_by_role("checkbox", name="收录「潜江」").check()
                expect(page.get_by_text("已勾选 2 / 2 条", exact=False)).to_be_visible()
                page.get_by_role("checkbox", name="收录「潜江」").uncheck()
                # Page shortcuts are ignored while a form field (the checkbox) has focus.
                page.get_by_role("list", name="词库 PR 列表").get_by_role("button").first.focus()
                page.keyboard.press("r")
                dialog = page.get_by_role("dialog", name="驳回 #9？")
                expect(dialog).to_be_visible()
                page.keyboard.press("Escape")
                expect(dialog).to_be_hidden()
                page.get_by_role("button", name="仅保留勾选项").click()
                expect(page.get_by_text("已在 #9 推送修改：只保留勾选的 1 条", exact=True)).to_be_visible()
                page.keyboard.press("a")
                expect(page.get_by_text("#9 已通过（1 条）", exact=True)).to_be_visible()
                page.get_by_role("button", name="撤销").click()
                expect(page.get_by_text("已撤销", exact=True)).to_be_visible()
                page.get_by_role("radio", name=re.compile("已通过")).click()
                expect(page.get_by_role("list", name="词库 PR 列表")).to_contain_text("#8")
                page.set_viewport_size({"width": 390, "height": 800})
                expect(page.get_by_role("list", name="词库 PR 列表")).to_be_visible()
                page.set_viewport_size({"width": 1280, "height": 860})
                violations("dictpr")

                page.goto(base + "/community")
                expect(page.get_by_text("命中敏感词：「加V」")).to_be_visible()
                page.get_by_role("button", name="春日樱", exact=True).click()
                drawer = page.get_by_role("dialog", name="春日樱")
                expect(drawer.get_by_role("img", name=re.compile("26 键"))).to_be_visible()
                expect(drawer.get_by_text("商标侵权：附截图")).to_be_visible()
                page.keyboard.press("Escape")
                expect(drawer).to_be_hidden()
                violations("community drawer")

                page.goto(base + "/words")
                words = page.get_by_role("table", name="敏感词列表")
                expect(words.get_by_text("/(微信|vx)[\\s:：]*[a-z0-9_-]{5,}/")).to_be_visible()
                expect(words.get_by_text("@owner", exact=False)).to_be_visible()
                # The fixture role lacks review_community, so the add bar and row actions are disabled.
                expect(page.get_by_role("button", name="添加")).to_be_disabled()
                page.get_by_role("radio", name=re.compile("^自定义")).click()
                expect(words.get_by_text("代购")).to_be_visible()
                expect(words.get_by_text("/(微信", exact=False)).to_be_hidden()
                page.goto(base + "/words?focus=2")
                expect(page.get_by_text("仅显示搜索定位的词条")).to_be_visible()
                expect(words.get_by_text("代购")).to_be_hidden()
                page.goto(base + "/words?focus=999")
                expect(page.get_by_text("搜索定位的词条已不在名单中")).to_be_visible()
                violations("/words")

                page.goto(base + "/release")
                expect(page.get_by_text("GitHub 暂时无法访问，请稍后重试。")).to_be_visible()
                page.get_by_role("button", name="发布历史").first.click()
                page.wait_for_url(re.compile(r"/release\?platform=windows$"))
                expect(page.get_by_text("剪贴板历史支持固定条目")).to_be_visible()
                page.get_by_role("button", name=re.compile("v0\\.5\\.3")).click()
                expect(page.get_by_text("缺少 vc_redist")).to_be_visible()
                expect(page.get_by_text("剪贴板历史支持固定条目")).to_be_hidden()
                page.goto(base + "/release?focus=windows:windows-v0.5.3")
                expect(page.get_by_text("缺少 vc_redist")).to_be_visible()
                violations("release")

                page.goto(base + "/")
                expect(page.locator("header")).to_contain_text("测试环境")
                # Overview (unit U11) renders every card from /api/overview and the shell's pending counts, chart included, at desktop and phone width.
                main = page.get_by_role("main")
                expect(main.get_by_text("4,821", exact=True)).to_be_visible()
                expect(main.get_by_text("99.62%", exact=True)).to_be_visible()
                expect(main.get_by_text("iOS 1.0.0 拖累", exact=True)).to_be_visible()
                expect(main.get_by_text("iOS · 内存超限", exact=True)).to_be_visible()
                expect(main.get_by_text("词库 PR 2 · 社区 0 · Issue 5", exact=True)).to_be_visible()
                expect(main.get_by_text("近 7 天收到 2 次举报", exact=True)).to_be_visible()
                expect(main.get_by_text("98.70% · 1.8s", exact=True)).to_be_visible()
                expect(main.locator(".recharts-surface").first).to_be_visible()
                page.set_viewport_size({"width": 390, "height": 800})
                expect(main.get_by_text("各平台活跃", exact=True)).to_be_visible()
                # The chart resizes on its ResizeObserver tick, so give the layout a moment to settle before judging overflow.
                try:
                    page.wait_for_function("document.documentElement.scrollWidth <= window.innerWidth", timeout=3000)
                except PlaywrightTimeout:
                    problems.append("overview scrolls horizontally at 390px")
                page.set_viewport_size({"width": 1280, "height": 860})
                main.get_by_role("link", name=re.compile("新增崩溃分组")).click()
                page.wait_for_url(base + "/crash")
                page.goto(base + "/")
                violations("overview")
                expect(page.get_by_role("link", name=re.compile("词库审核"))).to_contain_text("2")
                expect(page.get_by_text("部分服务降级")).to_be_visible()
                # The fixture role lacks view_cloud_usage, so 云端监控 is hidden from the sidebar.
                expect(page.get_by_role("complementary", name="后台导航").get_by_role("link", name="云端监控")).to_have_count(0)
                # 外壳没有报告 features.logs，服务日志同样不出现在侧栏。
                expect(page.get_by_role("complementary", name="后台导航").get_by_role("link", name="服务日志")).to_have_count(0)

                page.get_by_role("button", name=re.compile("^外观")).click()
                page.get_by_role("radio", name="深色").click()
                expect(page.locator("html")).to_have_attribute("data-theme", "dark")
                page.get_by_role("radio", name="冬").click()
                expect(page.locator("html")).to_have_attribute("data-season", "winter")
                page.keyboard.press("Escape")
                page.reload()
                expect(page.locator("html")).to_have_attribute("data-theme", "dark")
                expect(page.locator("html")).to_have_attribute("data-season", "winter")
                violations("appearance")

                page.get_by_role("button", name=re.compile("^通知")).click()
                expect(page.get_by_text("词库 PR #9 等待审核")).to_be_visible()
                page.get_by_role("button", name="全部已读").click()
                expect(page.get_by_text("已全部标记为已读", exact=True)).to_be_visible()
                page.keyboard.press("Escape")
                violations("notifications")

                page.keyboard.press("/")
                expect(page.get_by_placeholder("搜索页面、PR、Issue、用户…")).to_be_focused()
                page.keyboard.type("smoke")
                expect(page.get_by_role("option", name=re.compile("smoke-user"))).to_be_visible()
                page.keyboard.press("Enter")
                page.wait_for_url(re.compile(r"/users\?focus=u1$"))
                # The focus parameter opens the user drawer with the fixture detail; Escape closes it before the shell checks continue.
                drawer = page.get_by_role("dialog", name="smoke-user")
                expect(drawer).to_contain_text("MSIME/0.5.4 (Windows 11)")
                expect(drawer).to_contain_text("水杉秋色")
                page.keyboard.press("Escape")
                expect(drawer).to_be_hidden()
                expect(page.get_by_role("table", name="用户列表")).to_contain_text("解除封禁")
                violations("search")

                page.get_by_role("button", name="收起侧栏").click()
                expect(page.get_by_role("button", name="展开侧栏")).to_be_visible()
                page.reload()
                expect(page.get_by_role("button", name="展开侧栏")).to_be_visible()
                page.get_by_role("button", name="展开侧栏").click()

                page.set_viewport_size({"width": 390, "height": 800})
                expect(page.locator("#admin-navigation")).to_have_attribute("inert", "")
                page.get_by_role("button", name="打开导航").click()
                page.get_by_role("link", name=re.compile("系统状态")).first.click()
                page.wait_for_url(base + "/status")
                page.set_viewport_size({"width": 1280, "height": 860})
                violations("mobile nav")

                page.goto(base + "/issues")
                expect(page.get_by_role("table", name="Issue 列表")).to_contain_text("Shift 切换偶尔失效")
                expect(page.get_by_text("2.5 小时")).to_be_visible()
                page.get_by_text("Shift 切换偶尔失效").click()
                drawer = page.get_by_role("dialog", name=re.compile("#12 Shift 切换偶尔失效"))
                expect(drawer).to_contain_text("@houko 回复：需要日志")
                drawer.get_by_role("button", name="需要日志").click()
                expect(drawer.get_by_placeholder("回复提交者…")).to_have_value(re.compile("导出诊断日志"))
                page.keyboard.press("Escape")
                expect(drawer).to_be_hidden()
                page.goto(base + "/issues?focus=metasequoiaime%2Fmsime%2312")
                expect(page.get_by_role("dialog", name=re.compile("#12"))).to_be_visible()
                page.keyboard.press("Escape")
                violations("issues")

                page.goto(base + "/notice")
                expect(page.get_by_text("词库共建上线：打不出来的词直接提交")).to_be_visible()
                expect(page.get_by_role("button", name=re.compile("Telegram"))).to_be_disabled()
                page.get_by_text("Windows 10 工具栏图标方框的临时处理办法").click()
                page.get_by_role("button", name="编辑").click()
                expect(page.get_by_label("标题")).to_have_value("Windows 10 工具栏图标方框的临时处理办法")
                violations("notice")

                page.goto(base + "/perm")
                expect(page.get_by_role("button", name="运营/客服：封禁账号")).to_have_attribute("aria-pressed", "true")
                expect(page.get_by_text("收回「运营/客服」：封禁账号")).to_be_visible()
                expect(page.get_by_text("helper@example.com", exact=True)).to_be_visible()
                violations("perm")

                page.goto(base + "/me")
                expect(page.get_by_text("msime_pat_••••7c2e", exact=False)).to_be_visible()
                expect(page.get_by_text("通过了词库 PR #210（5 条）")).to_be_visible()
                expect(page.get_by_text("当前设备")).to_be_visible()
                # The badge follows /api/shell once the bell is closed, not the list cached when it was last open, and a notification preference change refreshes it.
                page.get_by_role("button", name=re.compile("^通知")).click()
                expect(page.get_by_text("词库 PR #9 等待审核")).to_be_visible()
                page.keyboard.press("Escape")
                FIXTURES["/api/shell"]["unread_notifications"] = 3
                try:
                    page.get_by_role("switch", name="崩溃告警").click()
                    expect(page.get_by_role("button", name="通知，3 条未读")).to_be_visible()
                finally:
                    FIXTURES["/api/shell"]["unread_notifications"] = 1
                violations("me")
                # A personal access token caller (via=token) must not be labelled as a Google session.
                page.route("**/api/me", lambda route: route.fulfill(json={**FIXTURES["/api/me"], "via": "token", "sessions": []}))
                page.goto(base + "/me")
                expect(page.get_by_text("owner@example.com · 个人访问令牌访问", exact=False)).to_be_visible()
                expect(page.get_by_text("令牌访问", exact=True)).to_be_visible()
                expect(page.get_by_text("Google 账号登录")).to_have_count(0)
                expect(page.get_by_text("两步验证与通行密钥由 Google 账号控制")).to_have_count(0)
                page.unroute("**/api/me")
                violations("me via token")
                page.goto(base + "/me")
                expect(page.get_by_text("两步验证与通行密钥由 Google 账号控制")).to_be_visible()
                page.get_by_role("button", name="退出登录").click()
                dialog = page.get_by_role("dialog", name="退出登录？")
                expect(dialog).to_be_visible()
                page.keyboard.press("Escape")
                expect(dialog).to_be_hidden()
                # Logging out inside a merge's 4s undo window sends the merge first, while the session is still valid.
                page.goto(base + "/dictpr")
                expect(page.get_by_role("list", name="#9 的词条")).to_contain_text("江汉油田")
                page.get_by_role("list", name="词库 PR 列表").get_by_role("button").first.focus()
                page.keyboard.press("a")
                expect(page.get_by_text("#9 已通过（1 条）", exact=True)).to_be_visible()
                page.locator('a[href="/me"]').first.click()
                page.wait_for_url(base + "/me")
                page.get_by_role("button", name="退出登录").click()
                dialog.get_by_role("button", name="退出").click()
                expect(page.get_by_role("heading", name="水杉管理后台")).to_be_visible()
                posts = [path for path in Handler.posts if path in ("/api/dict-prs/9/approve", "/api/auth/logout")]
                if posts != ["/api/dict-prs/9/approve", "/api/auth/logout"]:
                    problems.append(f"a merge pending at logout must be sent before auth/logout, got {posts}")
                violations("confirm + logout")

                page.goto(base + "/harness/")
                expect(page.get_by_role("table", name="测试表格")).to_be_visible()
                expect(page.locator(".recharts-surface").first).to_be_visible()
                page.get_by_role("checkbox", name="全选").check()
                expect(page.get_by_text("已选 3 项")).to_be_visible()
                page.get_by_role("button", name="批量驳回").click()
                dialog = page.get_by_role("dialog", name="驳回 3 项？")
                dialog.get_by_role("radio", name="原因乙").click()
                dialog.get_by_placeholder("补充说明（可选）").fill("备注")
                dialog.get_by_role("button", name="驳回").click()
                expect(page.get_by_text("已驳回：原因乙：备注", exact=True)).to_be_visible()
                expect(page.get_by_text("已选 3 项")).to_be_hidden()
                page.get_by_role("button", name="撤销").click()
                expect(page.get_by_text("已撤销", exact=True)).to_be_visible()
                page.get_by_role("checkbox", name="全选").check()
                page.get_by_role("button", name="批量失败").click()
                expect(page.get_by_text("操作失败：测试失败", exact=True)).to_be_visible()
                expect(page.get_by_text("已选 3 项")).to_be_visible()
                page.get_by_role("button", name="取消选择").click()
                page.get_by_role("button", name="延迟提交").click()
                page.get_by_role("button", name="撤销").click()
                expect(page.get_by_test_id("committed")).to_have_text("undone")
                page.get_by_role("button", name="延迟提交").click()
                expect(page.get_by_test_id("committed")).to_have_text("yes", timeout=6000)
                # A plain toast shown while a delayed commit waits neither sends the commit early nor takes away its 撤销.
                page.get_by_role("button", name="延迟提交").click()
                page.get_by_role("button", name="普通提示").click()
                expect(page.get_by_text("这是一条提示", exact=True)).to_be_visible()
                page.wait_for_timeout(300)
                expect(page.get_by_test_id("committed")).to_have_text("waiting")
                page.get_by_role("button", name="撤销").click()
                expect(page.get_by_test_id("committed")).to_have_text("undone")
                # Nor does the failure toast of an earlier commit that a newer delayed action sent.
                page.get_by_role("button", name="延迟失败").click()
                page.get_by_role("button", name="延迟提交").click()
                expect(page.get_by_text("操作失败：提交失败", exact=True)).to_be_visible()
                page.wait_for_timeout(300)
                expect(page.get_by_test_id("committed")).to_have_text("waiting")
                page.get_by_role("button", name="撤销").click()
                expect(page.get_by_test_id("committed")).to_have_text("undone")
                page.get_by_role("radio", name=re.compile("待处理")).click()
                expect(page.get_by_text("第三行")).to_be_hidden()
                page.get_by_text("第一行").click()
                drawer = page.get_by_role("dialog", name="详情")
                expect(drawer).to_be_visible()
                expect(drawer.get_by_role("img", name=re.compile("26 键"))).to_be_visible()
                page.keyboard.press("Escape")
                expect(drawer).to_be_hidden()
                # A toast raised from inside the drawer must not swallow Escape: the first press closes the drawer.
                page.get_by_role("button", name="打开抽屉").click()
                expect(drawer).to_be_visible()
                drawer.get_by_role("button", name="通过").click()
                expect(page.get_by_text("已通过", exact=True)).to_be_visible()
                page.keyboard.press("Escape")
                expect(drawer).to_be_hidden()
                # A toast shown before a modal opens keeps its 撤销 clickable for the pointer (the modal hides the rest of the page from assistive tech), and clicking it leaves the modal open.
                page.get_by_role("button", name="延迟提交").click()
                page.get_by_role("button", name="打开抽屉").click()
                expect(drawer).to_be_visible()
                page.locator("[data-msime-toast] button").click()
                expect(page.get_by_test_id("committed")).to_have_text("undone")
                expect(drawer).to_be_visible()
                page.keyboard.press("Escape")
                expect(drawer).to_be_hidden()
                # Escape closes the confirm dialog while a toast is on screen, whichever opened first.
                page.get_by_role("checkbox", name="全选").check()
                page.get_by_role("button", name="批量驳回").click()
                dialog = page.get_by_role("dialog", name=re.compile("驳回 \\d 项？"))
                expect(dialog).to_be_visible()
                page.keyboard.press("Escape")
                expect(dialog).to_be_hidden()
                page.get_by_role("button", name="延迟提交").click()
                page.get_by_role("button", name="批量驳回").click()
                expect(dialog).to_be_visible()
                page.keyboard.press("Escape")
                expect(dialog).to_be_hidden()
                page.get_by_role("button", name="取消选择").click()
                violations("ui harness")
            finally:
                browser.close()
    finally:
        server.shutdown()
        harness_dir.cleanup()

    if problems:
        print("\n".join(problems), file=sys.stderr)
        return 1
    print(f"CSP smoke passed: {len(PAGES)} pages, {len(REDIRECTS)} redirects, shell popovers, ui harness; CSP {Handler.csp!r}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
