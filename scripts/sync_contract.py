#!/usr/bin/env python3
"""从本仓 contracts/protocol.json 生成 Go 绑定，或检查生成结果。--source 可比较另一份契约。"""
import argparse
import json
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
parser = argparse.ArgumentParser()
parser.add_argument('--source', type=Path, help='要导入或比较的另一份契约路径')
parser.add_argument('--check', action='store_true')
args = parser.parse_args()
bundled = ROOT / 'contracts/protocol.json'
raw = (args.source or bundled).read_bytes()
spec = json.loads(raw)
if spec['version'] != '1':
    raise SystemExit('unsupported backend contract version')

def name(key):
    return ''.join(part.capitalize() for part in key.split('_'))

lines = ['// Code generated from contracts/protocol.json. DO NOT EDIT.',
         'package contract', '', 'const (', 'APIVersion = "1"']
for key, operation in (spec['operations'] | spec.get('websocket_operations', {})).items():
    lines += [f'{name(key)}Path = {json.dumps(operation["path"])}']
for key, value in spec['limits'].items():
    if not isinstance(value, int) or value <= 0:
        raise SystemExit('invalid protocol limit')
    lines += [f'{name(key)} = {value}']
lines += [')', '']
binding = subprocess.run(['gofmt'], input='\n'.join(lines).encode(), stdout=subprocess.PIPE, check=True).stdout
files = {bundled: raw, ROOT / 'internal/contract/contract.go': binding}
for path, data in files.items():
    if args.check:
        if not path.exists() or path.read_bytes() != data:
            sys.exit(f'contract mismatch: {path.relative_to(ROOT)}')
    else:
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data)
print('后端契约一致' if args.check else '已导入后端契约')
