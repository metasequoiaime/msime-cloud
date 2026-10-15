#!/usr/bin/env python3
"""CSP smoke test for the admin console build.

Serves admin-web/dist with the exact Content-Security-Policy that internal/server/admin.go sends, answers the shell's /api calls with fixed test fixtures, drives every route and the shell popovers in headless Chromium, and fails on any CSP violation, page error or missing shell element.

It also builds tests/harness (every shared ui component with test fixtures) into a temporary directory and exercises the data table, confirm dialog, toast, drawer and charts the same way.

Usage: python3 admin-web/tests/csp_smoke.py   (after `pnpm build`; needs pnpm and `pip install playwright && playwright install chromium`)
"""

import json
import datetime
import re
import subprocess
import sys
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

from playwright.sync_api import expect, sync_playwright

ROOT = Path(__file__).resolve().parents[2]
DIST = ROOT / "admin-web" / "dist"
ADMIN_GO = ROOT / "internal" / "server" / "admin.go"

PAGES = {
    "/": "数据概览", "/dictionary": "词库管理", "/dictpr": "词库审核", "/community": "社区审核", "/issues": "问题分诊", "/feedback": "用户反馈", "/words": "敏感词库",
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



# 词库管理使用合成词条，覆盖追加、词包编辑和来源预览。
DICTIONARY_FILES = {
    "custom/english.txt": [["example", "Example", "1"]],
    "custom/words.txt": [["水杉", "shui'shan", "5000"]],
    "custom/translations.txt": [["水杉", "old translation"]],
    "packs/programming/quanpin.txt": [["示例", "shi'li", "100"]] + [[f"测试{i}", "ce'shi", "100"] for i in range(104)],
    "sources/japanese/dictionary00.txt": [["れい", "1", "2", "300", "例"]],
}
FIXTURES["/api/dictionary"] = {
    "repo": "metasequoiaime/msime-dictionary", "branch": "main", "url": "https://github.com/metasequoiaime/msime-dictionary", "description": "词库源数据", "pushed_at": "2026-10-01T00:00:00Z",
    "files": [{"path": name, "sha": "a" * 40, "type": "blob", "size": 120} for name in DICTIONARY_FILES], "truncated": False,
    "counts": {"custom": 3, "packs": 1, "sources": 1}, "bytes": 600,
}
FIXTURES["/api/dictionary/changes"] = {"number": 42, "url": "https://github.com/metasequoiaime/msime-dictionary/pull/42", "branch": "dictionary-admin/synthetic"}


def dictionary_fixture(query):
    from urllib.parse import parse_qs
    params = parse_qs(query)
    name = params.get("path", ["custom/english.txt"])[0]
    rows = DICTIONARY_FILES.get(name, [])
    q = params.get("q", [""])[0].lower()
    filtered = [{"line": index + 1, "columns": row} for index, row in enumerate(rows) if q in "\t".join(row).lower()]
    page = int(params.get("page", ["1"])[0])
    return {"repo": "metasequoiaime/msime-dictionary", "name": name.split("/")[-1], "path": name, "sha": "a" * 40, "size": 120, "url": "https://github.com/metasequoiaime/msime-dictionary/blob/main/" + name,
            "rows": filtered[(page - 1) * 100:page * 100], "total": len(filtered), "page": page, "has_more": len(filtered) > page * 100, "encoding": "UTF-8", "editable": not name.startswith("sources/")}

def dictionary_search_fixture(query):
    from urllib.parse import parse_qs
    params = parse_qs(query)
    q = params.get("q", [""])[0].lower()
    scope = params.get("scope", ["all"])[0]
    files = sorted(name for name in DICTIONARY_FILES if scope == "all" or name.startswith(scope + "/"))
    cursor = int(params.get("cursor", ["0"])[0])
    end = min(cursor + 4, len(files))
    results = []
    for name in files[cursor:end]:
        rows = [{"line": index + 1, "columns": row} for index, row in enumerate(DICTIONARY_FILES[name]) if q in "\t".join(row).lower()]
        if rows:
            results.append({"path": name, "rows": rows[:100], "total": len(rows), "editable": not name.startswith("sources/")})
    return {"files": results, "skipped": [], "next_cursor": end, "has_more": end < len(files), "total_files": len(files), "revision": "c" * 40, "truncated": False}


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
        body = dictionary_fixture(query) if path == "/api/dictionary/file" else dictionary_search_fixture(query) if path == "/api/dictionary/search" else FIXTURES.get(path)
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


def check_bytes(page):
    # 直接调用三个页面的格式化函数，分别保留精度、舍入和特殊值行为。
    expected = [
        ["0", "0 B", "0 B", "0 B"],
        ["1.5", "1.5 B", "1.5 B", "1.5 B"],
        ["1023", "1023 B", "1023 B", "1023 B"],
        ["1024", "1.0 KB", "1.0 KB", "1 KB"],
        ["1536", "1.5 KB", "1.5 KB", "2 KB"],
        ["10239", "10.0 KB", "10.0 KB", "10 KB"],
        ["10240", "10.0 KB", "10 KB", "10 KB"],
        ["1048575", "1024.0 KB", "1024 KB", "1024 KB"],
        ["1048576", "1.0 MB", "1.0 MB", "1.0 MB"],
        ["1610612736", "1536.0 MB", "1536.0 MB", "1536.0 MB"],
        ["-1", "-1 B", "-1 B", "-1 B"],
        ["NaN", "NaN MB", "NaN MB", "NaN B"],
        ["Infinity", "Infinity MB", "Infinity MB", "Infinity MB"],
        ["-Infinity", "-Infinity B", "-Infinity B", "-Infinity B"],
    ]
    rows = page.get_by_test_id("byte-case")
    expect(rows).to_have_count(len(expected))
    for index, values in enumerate(expected):
        expect(rows.nth(index).locator("td")).to_have_text(values)


def check_debounce(browser, base: str, problems: list[str]):
    # Playwright 时钟作用于整个上下文；独立上下文避免暂停后影响其他页面的 Query 通知。
    context = browser.new_context(viewport={"width": 1280, "height": 860})
    context.add_init_script("window.__csp = []; document.addEventListener('securitypolicyviolation', e => window.__csp.push(e.violatedDirective + ' ' + (e.blockedURI || 'inline') + ' ' + (e.sourceFile || '') + ':' + e.lineNumber));")
    page = context.new_page()
    page.on("pageerror", lambda error: problems.append(f"debounce page error: {error}"))
    try:
        # 加载前安装可控时钟，连续输入的断言不依赖真实等待时间。
        page.clock.install(time=datetime.datetime(2026, 1, 1, 0, 0, tzinfo=datetime.timezone.utc))
        page.goto(base + "/harness/")
        value = page.get_by_test_id("debounced-value")
        expect(value).to_have_text("初始值")
        page.clock.pause_at(datetime.datetime(2026, 1, 1, 0, 1, tzinfo=datetime.timezone.utc))
        typed = page.get_by_role("textbox", name="防抖输入", exact=True)
        typed.fill("中间值")
        page.clock.run_for(199)
        expect(value).to_have_text("初始值")
        typed.fill("最终值")
        page.clock.run_for(199)
        expect(value).to_have_text("初始值")
        page.clock.run_for(1)
        expect(value).to_have_text("最终值")
        # 修改延迟后从新时长重新计时，旧定时器不能提前发布输入。
        typed.fill("调整延迟")
        page.clock.run_for(100)
        page.get_by_role("combobox", name="防抖延迟", exact=True).select_option("400")
        page.clock.run_for(399)
        expect(value).to_have_text("最终值")
        page.clock.run_for(1)
        expect(value).to_have_text("调整延迟")
        problems.extend(f"CSP violation on debounce harness: {item}" for item in page.evaluate("window.__csp.splice(0)"))
    finally:
        context.close()


def check_auth_generation(context, base: str, problems: list[str]):
    # 用受控响应模拟真实 mutation：清空 QueryClient 后，旧请求仍可能完成。
    for initial_token in (None, "test-admin-token"):
        page = context.new_page()
        page.on("pageerror", lambda error: problems.append(f"认证测试页面异常：{error}"))
        pending = []
        session = {**FIXTURES["/api/auth/session"], "token_enabled": True}
        page.route("**/api/auth/session", lambda route: route.fulfill(json=session))
        page.route("**/api/auth-test/delayed", lambda route: pending.append(route))
        page.route("**/api/auth-test/unauthorized", lambda route: route.fulfill(status=401, json={"error": "unauthorized"}))
        try:
            page.goto(base + "/harness/?auth")
            status = page.get_by_test_id("auth-status")
            expect(status).to_have_text("已登录")
            if initial_token:
                page.get_by_role("button", name="测试登录", exact=True).click()
                expect(page.get_by_test_id("auth-result")).to_have_text("登录完成")
                expect(page.get_by_test_id("auth-error")).to_be_empty()
            with page.expect_request("**/api/auth-test/delayed") as request:
                page.get_by_role("button", name="启动旧请求", exact=True).click()
            assert request.value.headers.get("authorization") == (f"Bearer {initial_token}" if initial_token else None)
            page.get_by_role("button", name="使当前登录失效", exact=True).click()
            expect(status).to_have_text("未登录")
            page.get_by_role("button", name="测试登录", exact=True).click()
            expect(page.get_by_test_id("auth-result")).to_have_text("登录完成")
            expect(status).to_have_text("已登录")
            expect(page.get_by_test_id("auth-error")).to_be_empty()
            assert len(pending) == 1
            pending.pop().fulfill(status=401, json={"error": "unauthorized"})
            expect(page.get_by_test_id("auth-result")).to_have_text("delayed:401")
            expect(status).to_have_text("已登录")
            expect(page.get_by_test_id("auth-error")).to_be_empty()
            # 新登录自己的 401 仍必须使当前身份退出。
            page.get_by_role("button", name="使当前登录失效", exact=True).click()
            expect(status).to_have_text("未登录")
            problems.extend(f"认证测试 CSP 违规：{item}" for item in page.evaluate("window.__csp.splice(0)"))
        finally:
            page.close()


def check_auth_logout(context, base: str, problems: list[str]):
    for response_status in (200, 500):
        page = context.new_page()
        page.on("pageerror", lambda error: problems.append(f"退出测试页面异常：{error}"))
        pending = []
        session = {**FIXTURES["/api/auth/session"], "token_enabled": True}
        page.route("**/api/auth/session", lambda route: route.fulfill(json=session))
        page.route("**/api/auth/logout", lambda route: pending.append(route))
        page.route("**/api/auth-test/unauthorized", lambda route: route.fulfill(status=401, json={"error": "unauthorized"}))
        try:
            page.goto(base + "/harness/?auth")
            status = page.get_by_test_id("auth-status")
            expect(status).to_have_text("已登录")
            with page.expect_request("**/api/auth/logout"):
                page.get_by_role("button", name="测试退出", exact=True).click()
            page.get_by_role("button", name="使当前登录失效", exact=True).click()
            expect(status).to_have_text("未登录")
            page.get_by_role("button", name="测试登录", exact=True).click()
            expect(page.get_by_test_id("auth-result")).to_have_text("登录完成")
            expect(status).to_have_text("已登录")
            assert len(pending) == 1
            pending.pop().fulfill(status=response_status, json={"ok": True} if response_status == 200 else {"error": "admin_auth_unavailable"})
            expect(page.get_by_test_id("auth-result")).to_have_text("退出完成")
            expect(status).to_have_text("已登录")
            expect(page.get_by_test_id("auth-error")).to_be_empty()
            # 当前身份发出的正常退出仍会清空登录状态。
            page.unroute("**/api/auth/logout")
            page.route("**/api/auth/logout", lambda route: route.fulfill(json={"ok": True}))
            page.get_by_role("button", name="测试退出", exact=True).click()
            expect(status).to_have_text("未登录")
            problems.extend(f"退出测试 CSP 违规：{item}" for item in page.evaluate("window.__csp.splice(0)"))
        finally:
            page.close()


def check_auth_logout_flush(context, base: str, problems: list[str]):
    page = context.new_page()
    page.on("pageerror", lambda error: problems.append(f"退出等待提交测试页面异常：{error}"))
    pending = []
    logout_requests = []
    session = {**FIXTURES["/api/auth/session"], "token_enabled": True}
    page.route("**/api/auth/session", lambda route: route.fulfill(json=session))
    page.route("**/api/auth-test/delayed", lambda route: pending.append(route))
    page.route("**/api/auth-test/unauthorized", lambda route: route.fulfill(status=401, json={"error": "unauthorized"}))

    def logout(route):
        logout_requests.append(route.request)
        route.fulfill(json={"ok": True})

    page.route("**/api/auth/logout", logout)
    try:
        page.goto(base + "/harness/?auth")
        status = page.get_by_test_id("auth-status")
        expect(status).to_have_text("已登录")
        page.get_by_role("button", name="排队延迟提交", exact=True).click()
        with page.expect_request("**/api/auth-test/delayed"):
            page.get_by_role("button", name="测试退出", exact=True).click()
        page.get_by_role("button", name="使当前登录失效", exact=True).click()
        expect(status).to_have_text("未登录")
        page.get_by_role("button", name="测试登录", exact=True).click()
        expect(page.get_by_test_id("auth-result")).to_have_text("登录完成")
        pending.pop().fulfill(json={"ok": True})
        expect(page.get_by_test_id("auth-result")).to_have_text("退出完成")
        assert not logout_requests, "身份已改变，不应再发送旧身份的退出请求"
        expect(status).to_have_text("已登录")
        problems.extend(f"退出等待提交测试 CSP 违规：{item}" for item in page.evaluate("window.__csp.splice(0)"))
    finally:
        page.close()


def wait_for_no_overflow(page, timeout_ms: int = 3000) -> bool:
    # 直接执行函数并逐帧检查布局，避免 wait_for_function 的轮询 eval 触发生产 CSP。
    return page.evaluate("""timeout => new Promise(resolve => {
        const deadline = performance.now() + timeout;
        const check = () => {
            const fits = document.documentElement.scrollWidth <= window.innerWidth;
            if (fits || performance.now() >= deadline) resolve(fits);
            else requestAnimationFrame(check);
        };
        check();
    })""", timeout_ms)


def check_profile_rollback(context, base: str, problems: list[str]):
    # 旧身份的失败 mutation 不应恢复到新身份的缓存；当前身份仍要正常撤销乐观更新。
    for change_identity, response_status in ((True, 401), (True, 500), (False, 500)):
        page = context.new_page()
        page.on("pageerror", lambda error: problems.append(f"个人资料回滚测试页面异常：{error}"))
        pending = []
        refresh = []
        hold_refresh = False
        session = {**FIXTURES["/api/auth/session"], "google_enabled": False, "token_enabled": True}
        old_profile = {**FIXTURES["/api/me"], "email": "old@example.test", "name": "旧管理员"}
        new_profile = {**FIXTURES["/api/me"], "email": "new@example.test", "name": "新管理员", "via": "token", "owner": False, "sessions": [], "token": None}
        page.route("**/api/auth/session", lambda route: route.fulfill(json=session))

        def me(route):
            if route.request.method == "POST":
                pending.append(route)
            elif hold_refresh:
                refresh.append(route)
            else:
                route.fulfill(json=new_profile if "authorization" in route.request.headers else old_profile)

        page.route("**/api/me", me)
        try:
            page.goto(base + "/me")
            expect(page.get_by_text("旧管理员", exact=True)).to_be_visible()
            preference = page.get_by_role("switch", name="词库 PR 提醒", exact=True)
            expect(preference).to_be_checked()
            with page.expect_request(lambda request: request.url.endswith("/api/me") and request.method == "POST"):
                preference.click()
            expect(preference).not_to_be_checked()
            if change_identity:
                page.get_by_role("button", name="退出登录", exact=True).click()
                page.get_by_role("dialog").get_by_role("button", name="退出", exact=True).click()
                page.get_by_label("管理员密钥", exact=True).fill("test-new-admin-token")
                page.get_by_role("button", name="进入控制台", exact=True).click()
                expect(page.get_by_text("新管理员", exact=True)).to_be_visible()
            hold_refresh = True
            pending.pop().fulfill(status=response_status, json={"error": "unauthorized" if response_status == 401 else "admin_auth_unavailable"})
            expect(page.get_by_text(re.compile(r"^操作失败："))).to_be_visible()
            if change_identity:
                expect(page.get_by_text("新管理员", exact=True)).to_be_visible()
                expect(page.get_by_text("旧管理员", exact=True)).to_have_count(0)
            else:
                expect(preference).to_be_checked()
                expect(page.get_by_text("旧管理员", exact=True)).to_be_visible()
            problems.extend(f"个人资料回滚测试 CSP 违规：{item}" for item in page.evaluate("window.__csp.splice(0)"))
        finally:
            for route in refresh:
                route.fulfill(json=new_profile if change_identity else old_profile)
            page.unroute_all(behavior="ignoreErrors")
            page.close()


def check_notice_pending(context, base: str, problems: list[str]):
    # 显式挂起请求，验证保存、发布和归档期间不能改写或切换编辑器；失败后仍保留原文。
    for action in ("save_notice_draft", "publish_notice", "archive_notice"):
        for status in (200, 500):
            page = context.new_page()
            page.on("pageerror", lambda error: problems.append(f"公告等待请求测试页面异常：{error}"))
            pending = []
            page.route("**/api/auth/session", lambda route: route.fulfill(json=FIXTURES["/api/auth/session"]))
            page.route("**/api/shell", lambda route: route.fulfill(json={**FIXTURES["/api/shell"], "me": {**FIXTURES["/api/shell"]["me"], "permissions": ["publish_notices"]}}))
            page.route("**/api/actions", lambda route: pending.append(route))
            try:
                page.goto(base + "/notice?focus=3")
                title = page.get_by_role("textbox", name="标题", exact=True)
                body = page.get_by_role("textbox", name="内容", exact=True)
                expect(title).to_have_value(FIXTURES["/api/notices"]["items"][1]["title"])
                title.fill("等待请求期间保留的标题")
                body.fill("等待请求期间保留的正文")
                live = page.locator("#notice-2")
                draft = page.locator("#notice-3")
                if action == "archive_notice":
                    draft.get_by_role("button", name="丢弃", exact=True).click()
                    trigger = page.get_by_role("dialog").get_by_role("button", name="丢弃", exact=True)
                else:
                    trigger = page.get_by_role("button", name="存为草稿" if action == "save_notice_draft" else "发布", exact=True)
                with page.expect_request(lambda request: request.url.endswith("/api/actions")) as sent:
                    trigger.click()
                assert sent.value.post_data_json["action"] == action
                assert sent.value.post_data_json["id"] == "3"
                expect(title).to_be_disabled()
                expect(body).to_be_disabled()
                expect(page.get_by_role("button", name="全部平台", exact=True)).to_be_disabled()
                expect(page.get_by_role("button", name="官网横幅", exact=True)).to_be_disabled()
                expect(page.get_by_role("button", name="取消编辑", exact=True)).to_be_disabled()
                expect(draft.get_by_role("button", name="编辑", exact=True)).to_be_disabled()
                expect(draft.get_by_role("button", name="丢弃", exact=True)).to_be_disabled()
                live.get_by_role("button").first.click()
                expect(live.get_by_role("button", name="归档", exact=True)).to_be_disabled()
                expect(title).to_have_value("等待请求期间保留的标题")
                expect(body).to_have_value("等待请求期间保留的正文")
                pending.pop().fulfill(status=status, json={"ok": True, "affected": 1} if status == 200 else {"error": "admin_auth_unavailable"})
                expect(title).to_be_enabled()
                expect(body).to_be_enabled()
                expect(live.get_by_role("button", name="归档", exact=True)).to_be_enabled()
                if status == 200:
                    expect(title).to_have_value("")
                    expect(body).to_have_value("")
                else:
                    expect(page.get_by_text(re.compile(r"^操作失败："))).to_be_visible()
                    expect(title).to_have_value("等待请求期间保留的标题")
                    expect(body).to_have_value("等待请求期间保留的正文")
                    expect(page.get_by_role("button", name="取消编辑", exact=True)).to_be_enabled()
                problems.extend(f"公告等待请求测试 CSP 违规：{item}" for item in page.evaluate("window.__csp.splice(0)"))
            finally:
                for route in pending:
                    route.abort()
                page.unroute_all(behavior="ignoreErrors")
                page.close()


def check_notice_pending_focus(context, base: str, problems: list[str]):
    # 全局搜索仍可使用，但不能让旧请求完成后的重置清空刚打开的另一条草稿。
    page = context.new_page()
    page.on("pageerror", lambda error: problems.append(f"公告延迟跳转测试页面异常：{error}"))
    pending = []
    other = {**FIXTURES["/api/notices"]["items"][1], "id": "4", "title": "另一条草稿", "body": "另一条正文"}
    page.route("**/api/auth/session", lambda route: route.fulfill(json=FIXTURES["/api/auth/session"]))
    page.route("**/api/notices", lambda route: route.fulfill(json={**FIXTURES["/api/notices"], "items": [*FIXTURES["/api/notices"]["items"], other]}))
    page.route("**/api/search?*", lambda route: route.fulfill(json={"items": [{"kind": "notice", "id": "4", "title": other["title"], "where": "公告", "target": "notice"}]}))
    page.route("**/api/actions", lambda route: pending.append(route))
    try:
        page.goto(base + "/notice?focus=3")
        title = page.get_by_role("textbox", name="标题", exact=True)
        expect(title).to_have_value(FIXTURES["/api/notices"]["items"][1]["title"])
        with page.expect_request(lambda request: request.url.endswith("/api/actions")):
            page.get_by_role("button", name="存为草稿", exact=True).click()
        expect(title).to_be_disabled()
        page.get_by_role("combobox", name="全局搜索").fill("另一条草稿")
        page.get_by_role("option", name="另一条草稿 公告", exact=True).click()
        expect(page).to_have_url(base + "/notice?focus=%224%22")
        expect(title).to_have_value(FIXTURES["/api/notices"]["items"][1]["title"])
        pending.pop().fulfill(json={"ok": True, "affected": 1})
        expect(title).to_be_enabled()
        expect(title).to_have_value(other["title"])
        expect(page.get_by_role("textbox", name="内容", exact=True)).to_have_value(other["body"])
        problems.extend(f"公告延迟跳转测试 CSP 违规：{item}" for item in page.evaluate("window.__csp.splice(0)"))
    finally:
        for route in pending:
            route.abort()
        page.unroute_all(behavior="ignoreErrors")
        page.close()


def check_mirror_pending(context, base: str, problems: list[str]):
    # 挂起保存和清空请求，避免旧响应丢掉等待期间的新输入；失败后应保留草稿供重试。
    for clearing in (False, True):
        for status in (200, 500):
            page = context.new_page()
            page.on("pageerror", lambda error: problems.append(f"镜像链接等待请求测试页面异常：{error}"))
            pending = []
            stored = dict(FIXTURES["/api/site-settings"])
            draft = "https://example.test/mirror-draft"
            page.route("**/api/auth/session", lambda route: route.fulfill(json=FIXTURES["/api/auth/session"]))
            page.route("**/api/shell", lambda route: route.fulfill(json={**FIXTURES["/api/shell"], "me": {**FIXTURES["/api/shell"]["me"], "permissions": ["publish_notices"]}}))

            def settings(route):
                if route.request.method == "POST":
                    pending.append(route)
                else:
                    route.fulfill(json=stored)

            page.route("**/api/site-settings", settings)
            try:
                page.goto(base + "/downloads")
                link = page.get_by_role("textbox", name="蓝奏云盘分享链接", exact=True)
                expect(link).to_have_value(stored["lanzou_url"])
                link.fill(draft)
                if clearing:
                    page.get_by_role("button", name="清空", exact=True).click()
                    trigger = page.get_by_role("dialog").get_by_role("button", name="清空", exact=True)
                else:
                    trigger = page.get_by_role("button", name="保存", exact=True)
                with page.expect_request(lambda request: request.url.endswith("/api/site-settings") and request.method == "POST") as sent:
                    trigger.click()
                submitted = "" if clearing else draft
                assert sent.value.post_data_json == {"lanzou_url": submitted}
                expect(link).to_be_disabled()
                expect(page.get_by_role("button", name="正在保存…", exact=True)).to_be_disabled()
                expect(page.get_by_role("button", name="清空", exact=True)).to_be_disabled()
                expect(link).to_have_value(draft)
                if status == 200:
                    stored["lanzou_url"] = submitted
                pending.pop().fulfill(status=status, json=stored if status == 200 else {"error": "admin_auth_unavailable"})
                expect(link).to_be_enabled()
                if status == 200:
                    expect(link).to_have_value(submitted)
                    expect(page.get_by_role("button", name="保存", exact=True)).to_be_disabled()
                else:
                    expect(page.get_by_text(re.compile(r"^保存失败："))).to_be_visible()
                    expect(link).to_have_value(draft)
                    expect(page.get_by_role("button", name="保存", exact=True)).to_be_enabled()
                    expect(page.get_by_role("button", name="清空", exact=True)).to_be_enabled()
                problems.extend(f"镜像链接等待请求测试 CSP 违规：{item}" for item in page.evaluate("window.__csp.splice(0)"))
            finally:
                for route in pending:
                    route.abort()
                page.unroute_all(behavior="ignoreErrors")
                page.close()


def check_pending_form_drafts(context, base: str, problems: list[str]):
    # 成功只清理已提交的内容；等待期间继续输入的新草稿和失败时的原文都应保留。
    cases = (
        ("/words", "敏感词或正则", "添加", "**/api/actions", {"ok": True, "affected": 1, "id": 55}, "测试词甲", "测试词乙", "已添加「测试词甲」"),
        ("/perm", "Google 账号邮箱", "添加管理员", "**/api/admins", {"ok": True, "affected": 1}, "first@example.test", "second@example.test", "已添加管理员 first"),
        ("/issues?focus=metasequoiaime%2Fmsime%2312", "回复提交者…", "发送", "**/api/issues/actions", {"ok": True, "affected": 1, "failed": []}, "先提交的回复", "等待期间新写的回复", "回复已发送"),
    )
    for path, label, button, endpoint, response, first, newer, success in cases:
        for editing in (True, False):
            for status in (200, 500):
                page = context.new_page()
                page.on("pageerror", lambda error: problems.append(f"表单草稿测试页面异常：{error}"))
                pending = []
                page.route("**/api/auth/session", lambda route: route.fulfill(json=FIXTURES["/api/auth/session"]))
                page.route("**/api/shell", lambda route: route.fulfill(json={**FIXTURES["/api/shell"], "me": {**FIXTURES["/api/shell"]["me"], "permissions": ["review_community", "manage_permissions", "triage_issues"]}}))
                page.route(endpoint, lambda route: pending.append(route))
                try:
                    page.goto(base + path)
                    field = page.get_by_role("textbox", name=label, exact=True)
                    field.fill(first)
                    with page.expect_request(lambda request: request.method == "POST") as sent:
                        page.get_by_role("button", name=button, exact=True).click()
                    payload = sent.value.post_data_json
                    submitted = payload["value"]["pattern"] if path == "/words" else payload["email"] if path == "/perm" else payload["body"]
                    assert submitted == first
                    expect(field).to_be_enabled()
                    if editing:
                        field.fill(newer)
                    pending.pop().fulfill(status=status, json=response if status == 200 else {"error": "admin_auth_unavailable"})
                    if status == 200:
                        expect(page.get_by_text(success, exact=True)).to_be_visible()
                    else:
                        expect(page.get_by_text(re.compile(r"^(添加失败|操作失败|发送失败)："))).to_be_visible()
                    expect(field).to_have_value(newer if editing else "" if status == 200 else first)
                    problems.extend(f"表单草稿测试 CSP 违规：{item}" for item in page.evaluate("window.__csp.splice(0)"))
                finally:
                    for route in pending:
                        route.abort()
                    page.unroute_all(behavior="ignoreErrors")
                    page.close()


def check_issue_reply_draft_switch(context, base: str, problems: list[str]):
    # 关闭后重新打开同一 Issue，或切到另一 Issue，即使新旧正文相同也不能清空新草稿。
    for target in (12, 13):
        for status in (200, 500):
            page = context.new_page()
            page.on("pageerror", lambda error: problems.append(f"Issue 草稿切换测试页面异常：{error}"))
            pending = []
            other = {**SMOKE_ISSUE, "number": 13, "title": "另一个问题", "url": "https://github.com/metasequoiaime/msime/issues/13"}
            page.route("**/api/auth/session", lambda route: route.fulfill(json=FIXTURES["/api/auth/session"]))
            page.route("**/api/shell", lambda route: route.fulfill(json={**FIXTURES["/api/shell"], "me": {**FIXTURES["/api/shell"]["me"], "permissions": ["triage_issues"]}}))
            page.route("**/api/issues?*", lambda route: route.fulfill(json={**FIXTURES["/api/issues"], "items": [SMOKE_ISSUE, other], "total": 2}))
            page.route("**/api/issues/metasequoiaime/msime/13", lambda route: route.fulfill(json={**FIXTURES["/api/issues/metasequoiaime/msime/12"], "issue": {**other, "body": "另一个问题的正文"}}))
            page.route("**/api/issues/actions", lambda route: pending.append(route))
            try:
                page.goto(base + "/issues?focus=metasequoiaime%2Fmsime%2312")
                field = page.get_by_role("textbox", name="回复提交者…", exact=True)
                field.fill("需要补充日志")
                with page.expect_request(lambda request: request.method == "POST") as sent:
                    page.get_by_role("button", name="发送", exact=True).click()
                assert sent.value.post_data_json["items"] == [{"repo": "metasequoiaime/msime", "n": 12}]
                page.get_by_role("button", name="关闭", exact=True).click()
                page.get_by_role("row").filter(has_text=f"#{target}").click()
                expect(page.get_by_role("dialog")).to_contain_text(f"#{target} ")
                expect(field).to_have_value("")
                field.fill("需要补充日志")
                pending.pop().fulfill(status=status, json={"ok": True, "affected": 1, "failed": []} if status == 200 else {"error": "admin_auth_unavailable"})
                expect(page.get_by_text("回复已发送", exact=True) if status == 200 else page.get_by_text(re.compile(r"^发送失败："))).to_be_visible()
                expect(field).to_have_value("需要补充日志")
                problems.extend(f"Issue 草稿切换测试 CSP 违规：{item}" for item in page.evaluate("window.__csp.splice(0)"))
            finally:
                for route in pending:
                    route.abort()
                page.unroute_all(behavior="ignoreErrors")
                page.close()


def check_release_pending(context, base: str, problems: list[str]):
    # 挂起发布操作，提交期间锁定正文；失败后保留原文并允许修改后重试。
    cases = (
        ("trigger", "版本号", "触发发布", "**/api/releases/windows/trigger", "version", "v0.5.5", "v0.5.6"),
        ("notes", "发布说明", "保存说明", "**/api/releases/windows/windows-v0.5.4/notes", "body", "### 修复\n- 已提交内容", "### 修复\n- 修改后重试"),
    )
    for kind, label, button, endpoint, key, draft, retry in cases:
        for status in (200, 500):
            page = context.new_page()
            page.on("pageerror", lambda error: problems.append(f"发布等待请求测试页面异常：{error}"))
            pending = []
            page.route("**/api/auth/session", lambda route: route.fulfill(json=FIXTURES["/api/auth/session"]))
            page.route("**/api/shell", lambda route: route.fulfill(json={**FIXTURES["/api/shell"], "me": {**FIXTURES["/api/shell"]["me"], "permissions": ["trigger_release"]}}))
            page.route(endpoint, lambda route: pending.append(route))
            try:
                page.goto(base + "/release?platform=windows")
                page.get_by_role("button", name="触发发布" if kind == "trigger" else "编辑说明", exact=True).click()
                dialog = page.get_by_role("dialog")
                field = dialog.get_by_role("textbox", name=label, exact=True)
                field.fill(draft)
                with page.expect_request(lambda request: request.method == "POST") as sent:
                    dialog.get_by_role("button", name=button, exact=True).click()
                assert sent.value.post_data_json == {key: draft}
                expect(field).to_be_disabled()
                expect(field).to_have_value(draft)
                expect(dialog.get_by_role("button", name="提交中…", exact=True)).to_be_disabled()
                expect(dialog.get_by_role("button", name="取消", exact=True)).to_be_disabled()
                page.keyboard.press("Escape")
                expect(dialog).to_be_visible()
                pending.pop().fulfill(status=status, json={"ok": True} if status == 200 else {"error": "admin_auth_unavailable"})
                if status != 200:
                    expect(page.get_by_text(re.compile(r"^操作失败："))).to_be_visible()
                    expect(field).to_be_enabled()
                    expect(field).to_have_value(draft)
                    expect(dialog.get_by_role("button", name="取消", exact=True)).to_be_enabled()
                    field.fill(retry)
                    with page.expect_request(lambda request: request.method == "POST") as resent:
                        dialog.get_by_role("button", name=button, exact=True).click()
                    assert resent.value.post_data_json == {key: retry}
                    expect(field).to_be_disabled()
                    pending.pop().fulfill(json={"ok": True})
                expect(dialog).to_have_count(0)
                success = f"Windows {retry if status != 200 else draft} 发布流水线已触发" if kind == "trigger" else "Windows v0.5.4 的发布说明已更新"
                expect(page.get_by_text(success, exact=True)).to_be_visible()
                problems.extend(f"发布等待请求测试 CSP 违规：{item}" for item in page.evaluate("window.__csp.splice(0)"))
            finally:
                for route in pending:
                    route.abort()
                page.unroute_all(behavior="ignoreErrors")
                page.close()


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

                page.goto(base + "/dictionary")
                table = page.get_by_role("grid", name="词条电子表格")

                def cell(address):
                    return table.locator(f'[data-cell="{address}"]')

                def select_cell(address):
                    cell(address).click()
                    return cell(address).get_by_role("textbox")

                def paste_cells(address, text):
                    target = select_cell(address)
                    target.evaluate("(element, text) => { const data = new DataTransfer(); data.setData('text/plain', text); element.dispatchEvent(new ClipboardEvent('paste', {bubbles: true, cancelable: true, clipboardData: data})); }", text)

                expect(cell("A1").get_by_role("textbox")).to_have_value("水杉")
                expect(cell("A1").get_by_role("textbox")).to_have_attribute("readonly", "")
                page.get_by_role("button", name="专注编辑", exact=True).click()
                expect(page.get_by_role("button", name="退出专注", exact=True)).to_be_visible()
                expect(page.get_by_role("textbox", name="搜索所有词库词条")).to_be_hidden()
                page.get_by_role("button", name="退出专注", exact=True).click()
                page.get_by_role("textbox", name="搜索当前文件词条").fill("missing")
                expect(page.get_by_text("未找到「missing」", exact=True)).to_be_visible()
                page.get_by_role("button", name="清空词条搜索").click()
                expect(cell("A1").get_by_role("textbox")).to_have_value("水杉")
                paste_cells("A1", "替换原词\tcode\t100\r\n附加词\tcode\t100")
                expect(page.get_by_role("alert")).to_contain_text("选区包含只读单元格")
                expect(cell("A3")).to_have_count(0)
                page.get_by_role("button", name="新增词条", exact=True).click()
                editor = cell("A2").get_by_role("textbox")
                expect(editor).to_be_focused()
                editor.fill("测试词")
                editor.press("Tab")
                expect(cell("B2").get_by_role("textbox")).to_be_focused()
                cell("B2").get_by_role("textbox").fill("ce'shi'ci")
                cell("B2").get_by_role("textbox").press("Tab")
                editor = cell("C2").get_by_role("textbox")
                editor.fill("5001")
                editor.dispatch_event("keydown", {"key": "Enter", "isComposing": True})
                expect(editor).to_be_focused()
                expect(page.get_by_text("正在输入", exact=True)).to_be_visible()
                with page.expect_request(lambda request: request.url.endswith("/api/dictionary/changes") and request.method == "POST") as saved:
                    editor.press("Control+s")
                assert saved.value.post_data_json["changes"] == [{"operation": "add", "line": 0, "columns": ["测试词", "ce'shi'ci", "5001"]}]
                expect(page.get_by_text("已创建 PR #42。", exact=False)).to_be_visible()

                page.get_by_role("radio", name=re.compile("专题词包")).click()
                expect(cell("A1").get_by_role("textbox")).to_have_value("示例")
                paste_cells("A100", "覆盖一\tcode\t100\r\n覆盖二\tcode\t100")
                expect(page.get_by_role("alert")).to_contain_text("粘贴区域跨过本页词条")
                expect(cell("A100").get_by_role("textbox")).to_have_value("测试98")
                page.get_by_role("button", name="下一页", exact=True).click()
                expect(page.get_by_text("第 2 / 2 页", exact=False)).to_be_visible()
                editor = select_cell("C1")
                editor.fill("123")
                editor.press("Enter")
                expect(page.get_by_text("待保存变更 · 1 项", exact=False)).to_be_visible()
                page.get_by_role("button", name="上一页", exact=True).click()
                page.get_by_role("button", name="下一页", exact=True).click()
                expect(select_cell("C1")).to_have_value("123")
                page.get_by_role("button", name="撤销", exact=True).click()
                expect(cell("C1").get_by_role("textbox")).to_have_value("100")
                cell("C1").get_by_role("textbox").press("Control+Shift+z")
                expect(cell("C1").get_by_role("textbox")).to_have_value("123")
                with page.expect_request(lambda request: request.url.endswith("/api/dictionary/changes") and request.method == "POST") as saved:
                    page.get_by_role("button", name="保存变更（1）", exact=True).click()
                assert saved.value.post_data_json["changes"][0]["line"] == 101
                expect(page.get_by_text("已创建 PR #42。", exact=False)).to_be_visible()

                page.get_by_role("textbox", name="搜索当前文件词条").fill("示例")
                expect(page.get_by_text("第 1 / 1 页", exact=False)).to_be_visible()
                editor = select_cell("C1")
                editor.press("F2")
                editor.fill("200")
                editor.press("Escape")
                expect(editor).to_have_value("100")
                # 直接打字覆盖，失焦暂存；内容栏和单元格保持同步。
                editor.press("2")
                editor.press("0")
                editor.press("0")
                select_cell("B1")
                expect(cell("C1")).to_have_text("200")
                select_cell("C1")
                formula = page.get_by_role("textbox", name="单元格内容", exact=True)
                formula.click()
                expect(page.get_by_text("正在输入", exact=True)).to_have_count(0)
                formula.press("Home")
                formula.press("ArrowRight")
                expect(formula).to_be_focused()
                formula.fill("201")
                formula.press("Tab")
                expect(select_cell("C1")).to_have_value("201")
                with page.expect_request(lambda request: request.url.endswith("/api/dictionary/changes") and request.method == "POST") as saved:
                    page.get_by_role("button", name="保存变更（1）", exact=True).click()
                assert saved.value.post_data_json["changes"] == [{"operation": "replace", "line": 1, "columns": ["示例", "shi'li", "201"]}]
                expect(page.get_by_text("已创建 PR #42。", exact=False)).to_be_visible()

                paste_cells("A2", "词一\tci'yi\t200\r\n词二\tci'er\t300\r\n")
                expect(page.get_by_text("待保存变更 · 2 项", exact=False)).to_be_visible()
                expect(cell("A2").get_by_role("textbox")).to_be_focused()
                expect(page.get_by_text("已粘贴 2 行 × 3 列 · 可撤销", exact=True)).to_be_visible()
                cell("A2").get_by_role("textbox").press("Control+z")
                expect(cell("A3")).to_have_count(0)
                cell("A2").get_by_role("textbox").press("Control+Shift+z")
                expect(cell("A3")).to_have_text("词二")
                select_cell("C2")
                cell("C3").click(modifiers=["Shift"])
                cell("C3").get_by_role("textbox").press("Control+d")
                expect(cell("C3").get_by_role("textbox")).to_have_value("200")
                select_cell("A2")
                cell("C3").click(modifiers=["Shift"])
                copied = cell("C3").get_by_role("textbox").evaluate("element => { const data = new DataTransfer(); element.dispatchEvent(new ClipboardEvent('copy', {bubbles: true, cancelable: true, clipboardData: data})); return data.getData('text/plain'); }")
                assert copied == "词一\tci'yi\t200\r\n词二\tci'er\t200"
                paste_cells("C2", "溢出\t字段")
                expect(page.get_by_role("alert")).to_contain_text("粘贴超出")
                expect(cell("C2").get_by_role("textbox")).to_have_value("200")
                editor = select_cell("B2")
                editor.fill("ci'yi'new")
                editor.press("Tab")
                expect(cell("A2")).to_have_text("词一")
                expect(cell("B2")).to_have_text("ci'yi'new")
                expect(cell("A3")).to_have_text("词二")
                select_cell("A2")
                page.get_by_role("button", name="删除选中行", exact=True).click()
                expect(cell("A2").get_by_role("textbox")).to_have_value("词二")
                page.get_by_role("button", name="撤销", exact=True).click()
                expect(cell("A2").get_by_role("textbox")).to_have_value("词一")
                page.get_by_role("button", name="放弃草稿", exact=True).click()

                select_cell("A1")
                page.get_by_role("button", name="删除选中行", exact=True).click()
                expect(cell("A1").locator("input")).to_have_class(re.compile("line-through"))
                page.get_by_role("button", name="撤销", exact=True).click()
                editor = select_cell("C1")
                editor.fill("201")
                editor.press("Enter")
                page.get_by_role("link", name="投稿审核", exact=True).click()
                expect(page.get_by_text("离开页面会放弃未提交的词条变更。")).to_be_visible()
                page.get_by_role("button", name="留在此页", exact=True).click()
                failed_batch = [False]
                def fail_search_batch_once(route):
                    if "cursor=4" in route.request.url and not failed_batch[0]:
                        failed_batch[0] = True
                        route.fulfill(status=503, content_type="application/json", body=json.dumps({"error": {"code": "github_unavailable"}}))
                    else:
                        route.continue_()
                page.route("**/api/dictionary/search*", fail_search_batch_once)
                page.get_by_role("textbox", name="搜索所有词库词条").fill("れい")
                page.get_by_role("button", name="搜索", exact=True).click()
                expect(page.get_by_text("搜索中断", exact=False)).to_be_visible()
                expect(page.get_by_text("4 / 5 个文件", exact=False)).to_be_visible()
                page.get_by_role("button", name="重试", exact=True).click()
                expect(page.get_by_text("搜索完成", exact=False)).to_be_visible()
                expect(page.get_by_text("5 / 5 个文件", exact=False)).to_be_visible()
                page.unroute("**/api/dictionary/search*", fail_search_batch_once)
                page.get_by_role("button", name="打开查看", exact=True).click()
                expect(page.get_by_text("当前文件有待提交变更", exact=False)).to_be_visible()
                page.get_by_role("button", name="放弃并切换").click()
                expect(cell("E1")).to_have_text("例")
                expect(select_cell("A1")).to_have_attribute("readonly", "")
                cell("A1").get_by_role("textbox").press("ArrowRight")
                expect(cell("B1").get_by_role("textbox")).to_be_focused()
                expect(page.get_by_role("columnheader", name=re.compile("词语"))).to_be_visible()
                expect(page.get_by_role("button", name="新增词条")).to_have_count(0)
                expect(page.get_by_role("textbox", name="搜索当前文件词条")).to_have_value("れい")
                page.get_by_role("button", name="关闭跨词库搜索").click()
                page.get_by_role("radio", name=re.compile("自定义词库")).click()
                page.get_by_role("button", name=re.compile("^translations.txt")).click()
                select_cell("B1").fill("new translation")
                cell("B1").get_by_role("textbox").press("Enter")
                with page.expect_request(lambda request: request.url.endswith("/api/dictionary/changes") and request.method == "POST") as saved:
                    page.get_by_role("button", name="保存变更（1）", exact=True).click()
                assert saved.value.post_data_json["changes"] == [{"operation": "add", "line": 0, "columns": ["水杉", "new translation"]}]
                page.get_by_role("textbox", name="搜索所有词库词条").fill("missing")
                page.get_by_role("button", name="搜索", exact=True).click()
                expect(page.get_by_text("未找到「missing」", exact=True)).to_be_visible()
                expect(page.get_by_text("已创建 PR #42。", exact=False)).to_be_visible()
                page.get_by_role("radio", name=re.compile("专题词包")).click()
                expect(cell("A1").get_by_role("textbox")).to_have_value("示例")
                editor = select_cell("A101")
                editor.fill("未完成词条")
                editor.press("Tab")
                expect(page.get_by_role("button", name="保存变更（1）", exact=True)).to_be_disabled()
                expect(page.get_by_text("1 行需修正", exact=False).last).to_be_visible()
                page.get_by_role("button", name="放弃草稿", exact=True).click()

                # 提交期间和版本冲突后只允许查看，避免旧草稿重复提交。
                held_posts = []
                def hold_dictionary_save(route):
                    held_posts.append(route)
                page.route("**/api/dictionary/changes", hold_dictionary_save)
                editor = select_cell("C1")
                editor.fill("250")
                editor.press("Control+s")
                expect(page.get_by_role("button", name="正在保存…", exact=True)).to_be_disabled()
                expect(editor).to_have_attribute("readonly", "")
                held_posts.pop().fulfill(status=409, content_type="application/json", body=json.dumps({"error": {"code": "dictionary_changed"}}))
                expect(page.get_by_text("文件已更新，当前草稿无法提交。", exact=False)).to_be_visible()
                expect(page.get_by_role("button", name="保存变更（1）", exact=True)).to_be_disabled()
                expect(page.get_by_role("button", name="撤销", exact=True)).to_be_disabled()
                expect(editor).to_have_attribute("readonly", "")
                page.get_by_role("button", name="放弃草稿", exact=True).click()
                editor = select_cell("C1")
                editor.fill("251")
                editor.press("Control+s")
                expect(page.get_by_role("button", name="正在保存…", exact=True)).to_be_disabled()
                held_posts.pop().fulfill(status=503, content_type="application/json", body=json.dumps({"error": {"code": "dictionary_uncertain"}}))
                expect(page.get_by_text("提交已停止", exact=False)).to_be_visible()
                expect(page.get_by_role("button", name="保存变更（1）", exact=True)).to_be_disabled()
                expect(editor).to_have_attribute("readonly", "")
                page.get_by_role("button", name="放弃草稿", exact=True).click()
                page.unroute("**/api/dictionary/changes", hold_dictionary_save)
                violations("dictionary editing and search")

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
                # 等待图表的 ResizeObserver 完成尺寸调整，再检查是否存在横向溢出。
                if not wait_for_no_overflow(page):
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
                check_bytes(page)
                check_debounce(browser, base, problems)
                check_auth_generation(context, base, problems)
                check_auth_logout(context, base, problems)
                check_auth_logout_flush(context, base, problems)
                check_profile_rollback(context, base, problems)
                check_notice_pending(context, base, problems)
                check_notice_pending_focus(context, base, problems)
                check_mirror_pending(context, base, problems)
                check_release_pending(context, base, problems)
                check_pending_form_drafts(context, base, problems)
                check_issue_reply_draft_switch(context, base, problems)
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
