#!/usr/bin/env python3
"""把已完成的 GitHub Release 封装并推送到社交分发 webhook。"""
import argparse
import hashlib
import hmac
import json
import os
import sys
import time
from pathlib import Path
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen


RELEASE_FIELDS = {
    "tagName": "tag",
    "name": "name",
    "body": "body",
    "url": "url",
    "publishedAt": "published_at",
    "targetCommitish": "target_commitish",
}


def build_payload(repo, tag, release):
    """校验 gh 的输出并构造稳定的 webhook 数据。"""
    if not isinstance(release, dict):
        raise ValueError("Release JSON 必须是对象")
    if release.get("tagName") != tag:
        raise ValueError("Release 标签与发布任务不一致")
    missing = [key for key in RELEASE_FIELDS if not isinstance(release.get(key), str)]
    if missing:
        raise ValueError("Release 缺少字段：" + ", ".join(missing))
    details = {output: release[input_name] for input_name, output in RELEASE_FIELDS.items()}
    return {
        "schema_version": 1,
        "event": "release.published",
        "idempotency_key": f"{repo}:{tag}",
        "repository": repo,
        "release": details,
    }


def encode_payload(payload):
    """使用固定格式编码，保证签名和重试时的请求正文一致。"""
    return json.dumps(payload, ensure_ascii=False, separators=(",", ":")).encode("utf-8")


def signature(body, secret):
    return "sha256=" + hmac.new(secret.encode("utf-8"), body, hashlib.sha256).hexdigest()


def post_payload(url, secret, payload, retries=3):
    """发送 webhook；只重试网络错误和 5xx，不把上游正文写进日志。"""
    body = encode_payload(payload)
    request = Request(
        url,
        data=body,
        headers={
            "Content-Type": "application/json",
            "User-Agent": "msime-cloud-release-announcer/1",
            "X-MSIME-Signature": signature(body, secret),
            "X-MSIME-Idempotency-Key": payload["idempotency_key"],
        },
        method="POST",
    )
    for attempt in range(retries):
        try:
            with urlopen(request, timeout=20) as response:
                status = response.status
            if 200 <= status < 300:
                return
            if status < 500 or attempt == retries - 1:
                raise RuntimeError(f"社交 webhook 返回 HTTP {status}")
        except HTTPError as error:
            if error.code < 500 or attempt == retries - 1:
                raise RuntimeError(f"社交 webhook 返回 HTTP {error.code}") from error
        except (URLError, TimeoutError, OSError) as error:
            if attempt == retries - 1:
                raise RuntimeError("社交 webhook 请求失败") from error
        time.sleep(2**attempt)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--release-json", required=True, type=Path)
    parser.add_argument("--repo", required=True)
    parser.add_argument("--tag", required=True)
    parser.add_argument("--url", default=os.environ.get("SOCIAL_WEBHOOK_URL", ""))
    parser.add_argument("--secret", default=os.environ.get("SOCIAL_WEBHOOK_SECRET", ""))
    args = parser.parse_args(argv)
    if not args.url or not args.secret:
        parser.error("必须配置 --url/--secret 或对应的环境变量")
    try:
        release = json.loads(args.release_json.read_text(encoding="utf-8"))
        payload = build_payload(args.repo, args.tag, release)
        post_payload(args.url, args.secret, payload)
    except (OSError, json.JSONDecodeError, ValueError, RuntimeError) as error:
        print(f"社交分发失败：{error}", file=sys.stderr)
        return 1
    print(f"社交分发已提交：{args.tag}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
