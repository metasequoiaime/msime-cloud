#!/usr/bin/env python3
"""基于已检出的、已验证的 main 提交发布版本。

验证期间 main 若前进，原子推送失败，不会发布未验证的代码。新提交若改了触发发布的路径，push 已为它在同一 concurrency 组里排上一次发布，那一次会验证并发布包含本次改动的新 main，本次让位并以成功结束；否则仍然失败，由人重跑。"""
import os
import re
import subprocess
from pathlib import Path

# backend-release.yml 的 on.push.paths，逐字一致（test_trigger_paths_mirror_the_workflow 守着）。
TRIGGER_PATHS = ['cmd/**', 'internal/**', 'admin-web/**', 'docs/**', '!**/*.md', 'version.go', 'go.mod', 'go.sum', 'Dockerfile',
                 '.dockerignore', '.gitmodules', 'THIRD_PARTY_NOTICES.txt', 'native/**', 'third_party/**',
                 'scripts/fetch_engine_resources.py', 'scripts/release/**',
                 '.github/workflows/backend-release.yml']

PATHS = ['cmd', 'internal', 'admin-web', 'docs', 'version.go', 'go.mod', 'go.sum', 'Dockerfile', '.dockerignore', '.gitmodules',
         'THIRD_PARTY_NOTICES.txt', 'native', 'third_party', 'scripts/fetch_engine_resources.py',
         'scripts/release', '.github/workflows/backend-release.yml',
         ':(exclude)**/*.md']


def git(*args):
    return subprocess.check_output(['git', *args], text=True).strip()


def superseded(base):
    """base 之后进入 main 的提交里，是否有会触发新一次发布的。"""
    git('fetch', '--quiet', 'origin', 'main')
    specs = [f':(glob,exclude){p[1:]}' if p.startswith('!') else f':(glob){p}' for p in TRIGGER_PATHS]
    return bool(git('log', '--format=%H', f'{base}..origin/main', '--', *specs))


def next_version(current, messages):
    major, minor, patch = map(int, current.split('.'))
    breaking = re.search(r'^[a-z]+(?:\([^)]*\))?!:|^BREAKING[ -]CHANGE:', messages, re.M)
    feature = re.search(r'^feat(?:\([^)]*\))?:', messages, re.M)
    if breaking and major:
        return f'{major + 1}.0.0'
    if breaking or feature:
        return f'{major}.{minor + 1}.0'
    return f'{major}.{minor}.{patch + 1}'


def release():
    if git('status', '--porcelain'):
        raise RuntimeError('工作区必须干净')
    base = git('rev-parse', 'HEAD')
    current = Path('VERSION').read_text().strip()
    if not re.fullmatch(r'(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)', current):
        raise RuntimeError('VERSION 必须是无前缀的正式语义版本')
    previous = f'backend-v{current}'
    tags = git('tag', '--list').splitlines()
    version = current
    reuse = False
    if previous in tags:
        subprocess.run(['git', 'merge-base', '--is-ancestor', previous, 'HEAD'], check=True)
        commits = git('log', '--format=%H', f'{previous}..HEAD', '--', *PATHS)
        if commits:
            messages = git('log', '--format=%s%n%b', f'{previous}..HEAD', '--', *PATHS)
            version = next_version(current, messages)
        else:
            reuse = True
    elif any(t.startswith('backend-v') for t in tags):
        raise RuntimeError('VERSION 与既有标签不一致，拒绝跳过历史版本')

    tag = f'backend-v{version}'
    if not reuse:
        if tag in tags:
            raise RuntimeError(f'{tag} 已存在，拒绝覆盖')
        if version != current:
            Path('VERSION').write_text(version + '\n')
            git('config', 'user.name', 'msime-release-bot')
            git('config', 'user.email', '41898282+github-actions[bot]@users.noreply.github.com')
            git('add', 'VERSION')
            git('commit', '-m', f'chore(release): backend v{version}')
        git('tag', tag)
        # 不强推；若验证期间 main 前进，整个推送失败，重跑会验证新 main。
        try:
            subprocess.run(['git', 'push', '--atomic', 'origin', 'HEAD:refs/heads/main',
                            f'refs/tags/{tag}:refs/tags/{tag}'], check=True)
        except subprocess.CalledProcessError:
            git('tag', '-d', tag)
            if superseded(base):
                print(f'验证期间 main 已前进，新提交会触发下一次发布并包含本次改动；{tag} 由那一次发布')
                return None
            raise
    revision = git('rev-parse', f'{tag}^{{commit}}')
    result = {'version': version, 'tag': tag, 'revision': revision}
    if os.environ.get('GITHUB_OUTPUT'):
        with open(os.environ['GITHUB_OUTPUT'], 'a') as out:
            out.writelines(f'{k}={v}\n' for k, v in result.items())
    print(f'发布目标：{tag} ({revision})' + ('，复用已有标签以恢复发布' if reuse else ''))
    return result


if __name__ == '__main__':
    release()
