#!/usr/bin/env python3
"""刷新 internal/skins/testdata/client_dialect.json，或用 --check 核对它是否与 msime 的共享用例表一致。

用例表的唯一来源是 msime 的 crates/client-core/src/skin/catalog/client_dialect.json，皮肤清单的各个实现（client-core、本仓的 internal/skins/client.go 与 scripts/candidate_skins_seed.py、msime-windows）都按它校验。msime 改了规则不会触动本仓，所以没有放进 PR 的必需检查：同步 msime 的规则改动时手动运行本脚本，再修正 go test 和种子脚本测试报出的差异。
"""
import argparse
import sys
import urllib.request
from pathlib import Path

SOURCE = 'https://raw.githubusercontent.com/metasequoiaime/msime/{ref}/crates/client-core/src/skin/catalog/client_dialect.json'
ROOT = Path(__file__).resolve().parents[1]
TARGET = ROOT / 'internal/skins/testdata/client_dialect.json'


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument('--ref', default='develop', help='要读取的 msime 分支、标签或提交（默认 develop）')
    parser.add_argument('--check', action='store_true', help='不一致时失败，不改写文件')
    args = parser.parse_args()
    with urllib.request.urlopen(SOURCE.format(ref=args.ref), timeout=30) as response:
        upstream = response.read()
    target = TARGET.relative_to(ROOT)
    if args.check:
        if TARGET.read_bytes() != upstream:
            print(f'{target} 与 msime {args.ref} 不一致；运行 scripts/sync_client_dialect.py 后修正测试报出的差异', file=sys.stderr)
            return 1
        print(f'{target} 与 msime {args.ref} 一致')
        return 0
    TARGET.write_bytes(upstream)
    print(f'已从 msime {args.ref} 写入 {target}')
    return 0


if __name__ == '__main__':
    sys.exit(main())
