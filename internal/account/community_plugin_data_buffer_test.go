package account

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"fmt"
	"hash/crc32"
	"testing"
)

// 直接写入成员头，既生成正常归档，也允许单独伪造长度和 CRC。
func pluginDataBufferArchive(tb testing.TB, name string, data []byte, method uint16, declared uint64, checksum uint32, truncated bool) []byte {
	tb.Helper()
	packed := data
	if method == zip.Deflate {
		var compressed bytes.Buffer
		w, err := flate.NewWriter(&compressed, flate.DefaultCompression)
		if err != nil {
			tb.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			tb.Fatal(err)
		}
		if err := w.Close(); err != nil {
			tb.Fatal(err)
		}
		packed = compressed.Bytes()
		if truncated {
			packed = packed[:len(packed)-1]
		}
	}
	var archive bytes.Buffer
	w := zip.NewWriter(&archive)
	out, err := w.CreateRaw(&zip.FileHeader{Name: name, Method: method, CRC32: checksum, CompressedSize64: uint64(len(packed)), UncompressedSize64: declared})
	if err != nil {
		tb.Fatal(err)
	}
	if _, err := out.Write(packed); err != nil {
		tb.Fatal(err)
	}
	if err := w.Close(); err != nil {
		tb.Fatal(err)
	}
	return archive.Bytes()
}

func TestPluginArchiveDataBufferContents(t *testing.T) {
	for _, name := range []string{"plugin.toml", "words.tsv", "NOTES.TXT", "click.wav"} {
		for _, size := range []int{0, 1, 511, 512, 513, 2048, maxPluginManifestBytes, maxPluginManifestBytes + 1, maxPluginDataBytes, maxPluginDataBytes + 1} {
			t.Run(fmt.Sprintf("%s/%d", name, size), func(t *testing.T) {
				data := bytes.Repeat([]byte{'a'}, size)
				archive := pluginDataBufferArchive(t, name, data, zip.Store, uint64(size), crc32.ChecksumIEEE(data), false)
				members, code := readPluginArchive(archive)
				if code != "" {
					t.Fatal(code)
				}
				member := members[name]
				if member.size != size || !bytes.Equal(member.head, data[:min(size, 512)]) {
					t.Fatal("成员长度或文件头改变")
				}
				keep := name == "plugin.toml" && size <= maxPluginManifestBytes || pluginDataExtension(name) && size <= maxPluginDataBytes
				if keep && (member.data == nil || !bytes.Equal(member.data, data)) || !keep && member.data != nil {
					t.Fatal("成员全文保留规则或内容改变")
				}
			})
		}
	}
}

func TestPluginArchiveDataBufferIntegrity(t *testing.T) {
	for _, method := range []uint16{zip.Store, zip.Deflate} {
		for _, size := range []int{0, 1, 511, 512, 513, 2048} {
			for _, change := range []string{"正常", "CRC错误", "长度偏大", "长度偏小", "嵌套归档", "压缩流截断"} {
				t.Run(fmt.Sprintf("%d/%d/%s", method, size, change), func(t *testing.T) {
					if change == "压缩流截断" && method != zip.Deflate {
						return
					}
					data := bytes.Repeat([]byte{'a'}, size)
					if change == "嵌套归档" {
						data = append([]byte("PK\x03\x04"), data...)
					}
					declared, checksum := uint64(len(data)), crc32.ChecksumIEEE(data)
					switch change {
					case "CRC错误":
						checksum ^= 1
					case "长度偏大":
						declared++
					case "长度偏小":
						if declared == 0 {
							return
						}
						declared--
					}
					archive := pluginDataBufferArchive(t, "words.tsv", data, method, declared, checksum, change == "压缩流截断")
					_, code := readPluginArchive(archive)
					want := "invalid_plugin_archive"
					if change == "正常" {
						want = ""
					}
					if code != want {
						t.Fatalf("分类=%q，期望=%q", code, want)
					}
				})
			}
		}
	}
}

func TestPluginArchiveDataBufferMemoryBudget(t *testing.T) {
	data := bytes.Repeat([]byte{'a'}, maxPluginDataBytes)
	archive := pluginDataBufferArchive(t, "words.tsv", data, zip.Store, uint64(len(data)), crc32.ChecksumIEEE(data), false)
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			members, code := readPluginArchive(archive)
			if code != "" || !bytes.Equal(members["words.tsv"].data, data) {
				b.Fatal("成员读取失败或内容改变：", code)
			}
		}
	})
	t.Logf("%s；%s", result, result.MemString())
	// 全文只需一个缓冲；为 ZIP 元数据、校验及 race 留余量，排除扩容和拼接产生的多份副本。
	if allocated := result.AllocedBytesPerOp(); allocated > maxPluginDataBytes+256<<10 {
		t.Fatalf("读取 4 MiB 数据分配 %d 字节，超过单份全文加 256 KiB 的预算", allocated)
	}
}

func BenchmarkPluginArchiveDataBuffer(b *testing.B) {
	for _, tc := range []struct {
		name string
		size int
	}{{"plugin.toml", maxPluginManifestBytes}, {"words.tsv", maxPluginDataBytes}} {
		data := bytes.Repeat([]byte{'a'}, tc.size)
		archive := pluginDataBufferArchive(b, tc.name, data, zip.Store, uint64(len(data)), crc32.ChecksumIEEE(data), false)
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				members, code := readPluginArchive(archive)
				if code != "" || !bytes.Equal(members[tc.name].data, data) {
					b.Fatal("成员读取失败或内容改变：", code)
				}
			}
		})
	}
}
