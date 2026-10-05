#!/usr/bin/env python3
"""下载并校验客户端固定的公共词库发布；不覆盖已有文件或读取客户端用户词库。

词库与客户端同源：发布文件、地址、长度和 SHA-256 都取自 third_party/msime 子模块里桌面客户端的锁文件 resources/desktop-dictionary.lock.json，辅助码表取自同一子模块的 resources/helpcodes。子模块的 commit 决定引擎与词库的版本，两者一起升级。
"""
import argparse
import hashlib
import json
from pathlib import Path
import shutil
import tempfile
import time
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
MSIME = ROOT/'third_party/msime'
LOCK = MSIME/'resources/desktop-dictionary.lock.json'
HELPCODES = MSIME/'resources/helpcodes'
MANIFEST = 'msime-dictionary-manifest.json'

# GitHub 的下载偶尔整段时间返回 504，2026-09-14 那次持续了二十来分钟，同一个窗口里三个仓库的 CI 全挂在各自的下载上。一次失败就退出等于把 CI 的成败绑在对方那几分钟的可用性上，而这里下载的内容有摘要校验，重试不会引入坏数据。
RETRY_STATUS = frozenset({408, 425, 429, 500, 502, 503, 504})
ATTEMPTS = 5


def open_with_retry(request):
    for attempt in range(1, ATTEMPTS + 1):
        try:
            return urllib.request.urlopen(request, timeout=60)
        except urllib.error.HTTPError as error:
            # 404、403 这些重试多少次都是同一个答案，立刻失败比等五轮退避有用。
            if error.code not in RETRY_STATUS or attempt == ATTEMPTS:
                raise
            reason = f'HTTP {error.code}'
        except (urllib.error.URLError, TimeoutError, ConnectionError) as error:
            if attempt == ATTEMPTS:
                raise
            reason = str(error)
        delay = 2 ** attempt
        print(f'下载失败（{reason}），{delay}s 后重试（第 {attempt}/{ATTEMPTS - 1} 次）', flush=True)
        time.sleep(delay)


def sha256(path):
    digest = hashlib.sha256()
    with path.open('rb') as source:
        while chunk := source.read(1024*1024):
            digest.update(chunk)
    return digest.hexdigest()


def fetch(artifact, destination):
    target = destination/artifact['name']
    if target.exists():
        if target.stat().st_size != artifact['size'] or sha256(target) != artifact['sha256']:
            raise SystemExit(f'已有资源摘要不匹配：{artifact["name"]}；请选择新的资源目录')
        return
    request = urllib.request.Request(artifact['url'], headers={'User-Agent': 'MSIME-Backend-resource-fetch'})
    with tempfile.NamedTemporaryFile(dir=destination, prefix='.download-', delete=False) as output:
        temporary = Path(output.name)
        try:
            with open_with_retry(request) as response:
                digest = hashlib.sha256()
                size = 0
                while chunk := response.read(1024*1024):
                    size += len(chunk)
                    if size > artifact['size']:
                        raise ValueError(f'资源大小超出锁文件：{artifact["name"]}')
                    digest.update(chunk)
                    output.write(chunk)
            output.close()
            if size != artifact['size'] or digest.hexdigest() != artifact['sha256']:
                raise ValueError(f'资源摘要不匹配：{artifact["name"]}')
            temporary.replace(target)
        finally:
            temporary.unlink(missing_ok=True)


def copy_checked(source, target):
    """Copy a repository file, refusing to replace a different one already there."""
    if target.exists():
        if target.read_bytes() != source.read_bytes():
            raise SystemExit(f'已有源码资源不匹配：{target.name}；请选择新的资源目录')
        return
    target.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(source, target)


def download(destination):
    if not LOCK.is_file():
        raise SystemExit('缺少 third_party/msime；先运行 git submodule update --init')
    lock = json.loads(LOCK.read_text())
    destination.mkdir(parents=True, exist_ok=True)
    for artifact in lock['artifacts']:
        fetch(artifact, destination)
    for source in sorted(HELPCODES.iterdir()):
        if source.is_file():
            copy_checked(source, destination/'helpcodes'/source.name)
    manifest = json.loads((destination/MANIFEST).read_text())
    if manifest['source']['commit'] != lock['source_commit'] or manifest['format_version'] != 1:
        raise SystemExit('词库来源或格式不匹配')
    print('固定词库来源与 SHA-256 校验通过')


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('destination', type=Path)
    download(parser.parse_args().destination)
