#!/usr/bin/env python3
"""Sync the QQ bot's group command panel (the list shown after typing "@bot /") from qq_command_panel.json.

The open platform console has no form for the panel; it is managed through the panel API. Run after changing the slash commands in internal/server/qq_slash.go:

    QQ_BOT_SECRET=... scripts/qq_command_panel.py            # create or update
    QQ_BOT_SECRET=... scripts/qq_command_panel.py --dry-run  # show the current panel only
"""
import argparse
import json
import os
from pathlib import Path
import sys
import urllib.error
import urllib.request

API = "https://api.sgroup.qq.com"
CONFIG = Path(__file__).with_name("qq_command_panel.json")


def call(method, url, body=None, token=None):
    request = urllib.request.Request(url, data=None if body is None else json.dumps(body).encode(), method=method, headers={"Content-Type": "application/json", "User-Agent": "msime-cloud-scripts"})
    if token:
        request.add_header("Authorization", "QQBot " + token)
    try:
        with urllib.request.urlopen(request, timeout=20) as response:
            return json.loads(response.read() or b"{}")
    except urllib.error.HTTPError as error:
        sys.exit(f"{method} {url}: {error.code} {error.read().decode()[:300]}")


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--dry-run", action="store_true", help="print the current group panel without changing it")
    args = parser.parse_args()
    secret = os.environ.get("QQ_BOT_SECRET") or sys.exit("QQ_BOT_SECRET is not set")
    token = call("POST", "https://bots.qq.com/app/getAppAccessToken", {"appId": os.environ.get("QQ_BOT_APPID", "1903463797"), "clientSecret": secret})["access_token"]
    panel = json.loads(CONFIG.read_text())
    records = call("GET", API + "/v2/panels?scope=group&limit=50", token=token).get("records", [])
    ours = [r for r in records if r.get("target_type") == "all"]
    if args.dry_run:
        print(json.dumps(ours, ensure_ascii=False, indent=2))
        return
    if ours:
        result = call("PUT", API + "/v2/panels/" + ours[0]["panel_id"], {"panel": panel}, token)
        print("updated", ours[0]["panel_id"], "version", result.get("version"))
    else:
        result = call("POST", API + "/v2/panels", {"scope": "group", "target_type": "all", "panel": panel}, token)
        print("created", result.get("panel_id"))


if __name__ == "__main__":
    main()
