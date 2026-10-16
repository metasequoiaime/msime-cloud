package account

import (
	"fmt"
	"strings"
	"testing"
)

var pluginExtensionFoldKinds = []string{"wav", "ogg", "txt", "tsv", "md", "zip", "jar", "apk", "7z", "rar", "tar", "gz", "tgz", "bz2", "xz", "zst", "lz", "lzma", "cab"}

// 固定原有任意扩展名的小写转换，验证常见扩展名优化不改变未知名称或 Unicode 规则。
func previousPluginExtensionFold(name string) string {
	i := strings.LastIndexByte(name, '.')
	if i < 0 {
		return ""
	}
	return strings.ToLower(name[i+1:])
}

func TestPluginExtensionFoldCompatibility(t *testing.T) {
	check := func(name string) {
		t.Helper()
		want := previousPluginExtensionFold(name)
		if got := pluginExtension(name); got != want {
			t.Fatalf("扩展名 %q：%q，期望 %q", name, got, want)
		}
		if got := pluginAudio(name); got != (want == "wav" || want == "ogg") {
			t.Fatalf("音频类型不符：%q", name)
		}
		if got := pluginDataExtension(name); got != (want == "txt" || want == "tsv") {
			t.Fatalf("数据类型不符：%q", name)
		}
		if got := pluginNotice(name); got != (want == "txt" || want == "md") {
			t.Fatalf("说明类型不符：%q", name)
		}
		nested := false
		switch want {
		case "zip", "jar", "apk", "7z", "rar", "tar", "gz", "tgz", "bz2", "xz", "zst", "lz", "lzma", "cab":
			nested = true
		}
		if got := pluginNestedArchive(name, nil); got != nested {
			t.Fatalf("嵌套归档判断不符：%q", name)
		}
	}
	for _, ext := range append(append([]string{}, pluginExtensionFoldKinds...), "foo", "abcd", "abcde", "", "ZİP", "APK", "ZſT", "ŴAV", "ＴＸＴ", "\xffA", "Σ", "\x00X") {
		for bits := 0; bits < 1<<min(len(ext), 5); bits++ {
			variant := []byte(ext)
			for i, c := range variant {
				if i < 5 && bits&(1<<i) != 0 && c >= 'a' && c <= 'z' {
					variant[i] = c - 'a' + 'A'
				}
			}
			check("name." + string(variant))
			check(".hidden." + string(variant))
		}
	}
	for _, template := range []string{"name.TXT", "name.LZMA", "name.FOO", "name.ABCDE"} {
		name := []byte(template)
		for pos, original := range name {
			for value := 0; value < 256; value++ {
				name[pos] = byte(value)
				check(string(name))
			}
			name[pos] = original
		}
	}
	for _, name := range []string{"", "README", "name.", "name..TXT", "name．TXT", "name.\x80", "name." + strings.Repeat("X", 65535), "name." + strings.Repeat("界", 1000)} {
		check(name)
	}
	for _, ext := range []string{"txt", "tsv", "foo", "FOO", "", "abcde"} {
		name := "data." + strings.ToUpper(ext)
		if got, want := pluginDataName(&name, ext), validPluginFileName(name) && previousPluginExtensionFold(name) == ext; got != want {
			t.Fatalf("点名数据文件 %q，类型 %q：%t，期望 %t", name, ext, got, want)
		}
	}
}

func TestPluginExtensionFoldAllocationBudget(t *testing.T) {
	for _, ext := range pluginExtensionFoldKinds {
		name := "file." + strings.ToUpper(ext)
		var got string
		allocations := testing.AllocsPerRun(100, func() { got = pluginExtension(name) })
		if got != ext || allocations != 0 {
			t.Errorf("扩展名 %q：结果 %q，分配 %.0f 次，期望 %q 和零分配", name, got, allocations, ext)
		}
	}
}

func pluginExtensionFoldArchive(tb testing.TB, upper bool) ([]byte, []string) {
	tb.Helper()
	var names []string
	contents := map[string][]byte{}
	for i := 0; i < maxPluginPackFiles; i++ {
		ext := "txt"
		if upper {
			ext = "TXT"
		}
		name := fmt.Sprintf("file%02d.%s", i, ext)
		names = append(names, name)
		contents[name] = []byte("synthetic fixture")
	}
	archive, err := pluginSeedArchive(names, contents)
	if err != nil {
		tb.Fatal(err)
	}
	return archive, names
}

func TestPluginExtensionFoldArchive(t *testing.T) {
	var allocations [2]float64
	for variant, upper := range []bool{false, true} {
		archive, names := pluginExtensionFoldArchive(t, upper)
		allocations[variant] = testing.AllocsPerRun(100, func() {
			members, code := readPluginArchive(archive)
			if code != "" || len(members) != len(names) {
				t.Fatalf("归档结果不符：%d 个成员，错误码 %q", len(members), code)
			}
			for _, name := range names {
				member, ok := members[name]
				if !ok || member.size != len("synthetic fixture") || string(member.head) != "synthetic fixture" || string(member.data) != "synthetic fixture" {
					t.Fatalf("未保留原文件名或完整数据：%q", name)
				}
			}
		})
	}
	// 大写去重键仍按每个文件分配一次；为标准库缓冲池波动保留余量。
	if delta := allocations[1] - allocations[0]; delta > 24 {
		t.Errorf("大写扩展名额外分配 %.0f 次，预算 24 次", delta)
	}
	t.Logf("小写 %.0f 次，大写 %.0f 次", allocations[0], allocations[1])
	for _, ext := range []string{"ZIP", "ZİP", "APK"} {
		archive := pluginZip(t, pluginFile{name: ".hidden/file." + ext, data: "synthetic fixture"})
		if _, code := readPluginArchive(archive); code != "invalid_plugin_archive" {
			t.Fatalf("隐藏嵌套归档 %q：错误码 %q", ext, code)
		}
	}
	archive := pluginZip(t, pluginFile{name: ".hidden/file.ZſT", data: "synthetic fixture"})
	if members, code := readPluginArchive(archive); code != "" || len(members) != 0 {
		t.Fatalf("Unicode 小写规则被改变：%d 个成员，错误码 %q", len(members), code)
	}
}

func BenchmarkPluginExtensionFold(b *testing.B) {
	for _, tc := range []struct{ name, input string }{
		{"lower", "file.txt"}, {"upper", "file.TXT"}, {"unknown", "file.FOO"}, {"unicode", "file.ZİP"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			want := previousPluginExtensionFold(tc.input)
			b.ReportAllocs()
			for b.Loop() {
				if pluginExtension(tc.input) != want {
					b.Fatal("扩展名结果不符")
				}
			}
		})
	}
	for _, upper := range []bool{false, true} {
		name := "archive/lower"
		if upper {
			name = "archive/upper"
		}
		b.Run(name, func(b *testing.B) {
			archive, names := pluginExtensionFoldArchive(b, upper)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				members, code := readPluginArchive(archive)
				if code != "" || len(members) != len(names) {
					b.Fatal("归档结果不符", code)
				}
			}
		})
	}
}
