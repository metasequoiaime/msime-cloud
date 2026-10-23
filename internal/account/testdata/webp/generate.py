"""用本地 cwebp 1.6.0 生成 128×128 合成噪声样本；Go 测试只读取生成结果。"""

from pathlib import Path
import struct
import subprocess
import tempfile
import zlib


def chunk(kind, data):
    return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data))


root = Path(__file__).resolve().parent
# 固定的 xorshift32 字节流保证样本生成不依赖外部素材或随机种子。
state = 0x31415926
pixels = bytearray()
for _ in range(128 * 128 * 4):
    state ^= (state << 13) & 0xFFFFFFFF
    state ^= state >> 17
    state ^= (state << 5) & 0xFFFFFFFF
    pixels.append(state & 255)
rows = b"".join(b"\0" + pixels[y * 512:(y + 1) * 512] for y in range(128))
header = struct.pack(">IIBBBBB", 128, 128, 8, 6, 0, 0, 0)
png = b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", header) + chunk(b"IDAT", zlib.compress(rows)) + chunk(b"IEND", b"")
with tempfile.TemporaryDirectory(prefix="msime-webp-fixtures-") as directory:
    source = Path(directory) / "noise.png"
    source.write_bytes(png)
    for name, options in (("noise-alpha.webp", ["-q", "75"]), ("noise-lossless.webp", ["-lossless"])):
        subprocess.run(["cwebp", "-quiet", "-exact", *options, str(source), "-o", str(root / name)], check=True)
