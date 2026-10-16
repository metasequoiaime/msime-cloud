package account

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// 固定原有分段与遍历规则；归档数据均为普通短文本，仅比较路径决策。
func previousPluginPathScanNames(files []pluginFile) ([]string, string) {
	if len(files) > maxPluginArchiveMembers {
		return nil, "plugin_too_large"
	}
	var entries [][]string
	for _, file := range files {
		name := file.name
		if name == "" || !utf8.ValidString(name) || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\:") || strings.ContainsFunc(name, unicode.IsControl) {
			return nil, "invalid_plugin_archive"
		}
		dir := strings.HasSuffix(name, "/")
		parts := strings.Split(strings.TrimSuffix(name, "/"), "/")
		for _, part := range parts {
			if part == "" || part == "." || part == ".." {
				return nil, "invalid_plugin_archive"
			}
		}
		if file.mode&fs.ModeSymlink != 0 || file.mode.Type()&^fs.ModeDir != 0 || file.flags&1 != 0 || pluginNestedArchive(parts[len(parts)-1], nil) {
			return nil, "invalid_plugin_archive"
		}
		if parts[0] == "__MACOSX" || slices.ContainsFunc(parts, func(part string) bool { return strings.HasPrefix(part, ".") }) {
			continue
		}
		if dir || file.mode.IsDir() {
			if len(parts) > 1 {
				return nil, "invalid_plugin_archive"
			}
			continue
		}
		entries = append(entries, parts)
	}
	wrapper := ""
	if len(entries) > 0 && len(entries[0]) == 2 {
		wrapper = entries[0][0]
	}
	if len(entries) > maxPluginPackFiles {
		return nil, "plugin_too_large"
	}
	var names []string
	seen := map[string]bool{}
	for _, parts := range entries {
		var name string
		switch {
		case wrapper == "" && len(parts) == 1:
			name = parts[0]
		case wrapper != "" && len(parts) == 2 && parts[0] == wrapper:
			name = parts[1]
		default:
			return nil, "invalid_plugin_archive"
		}
		if !validPluginFileName(name) || seen[strings.ToLower(name)] {
			return nil, "invalid_plugin_archive"
		}
		seen[strings.ToLower(name)] = true
		names = append(names, name)
	}
	slices.Sort(names)
	return names, ""
}

func pluginPathScanArchive(tb testing.TB, files []pluginFile) []byte {
	tb.Helper()
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for _, file := range files {
		header := &zip.FileHeader{Name: file.name, Method: zip.Store, Flags: file.flags}
		if file.mode != 0 {
			header.SetMode(file.mode)
		}
		out, err := writer.CreateHeader(header)
		if err != nil {
			tb.Fatal(err)
		}
		if !strings.HasSuffix(file.name, "/") {
			if _, err := out.Write([]byte("synthetic fixture")); err != nil {
				tb.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		tb.Fatal(err)
	}
	return buf.Bytes()
}

func TestPluginPathScanCompatibility(t *testing.T) {
	check := func(files []pluginFile) {
		t.Helper()
		want, wantCode := previousPluginPathScanNames(files)
		members, code := readPluginArchive(pluginPathScanArchive(t, files))
		var got []string
		for name, member := range members {
			got = append(got, name)
			if member.size != len("synthetic fixture") || string(member.head) != "synthetic fixture" {
				t.Fatalf("未保留文件内容：%q", name)
			}
		}
		slices.Sort(got)
		if code != wantCode || !slices.Equal(got, want) {
			t.Fatalf("路径 %+v：成员 %q、错误码 %q，期望 %q、%q", files, got, code, want, wantCode)
		}
	}
	for _, name := range []string{"", "/", "a/", "a//", "a//b.txt", ".", "..", "a/./b.txt", "a/../b.txt", "a/b.txt", "a/b/c.txt", "a/b/", "a/.hidden/c.txt", ".hidden/a/b.txt", "__MACOSX/a/b.txt", "__MACOSX/a/b.zip", "a/__MACOSX/c.txt", "a/../.hidden/c.txt", "a/.../c.txt", "包装/readme.txt", "包装/.隐藏/readme.txt", "a/\u0085/readme.txt", "a/\u2028/readme.txt", "a\\b.txt", "C:readme.txt", "a/CON.txt", "a/Readme.txt", "a/\xff.txt", strings.Repeat("x/", 10000) + ".hidden/readme.txt"} {
		check([]pluginFile{{name: name}})
	}
	for _, template := range []string{"pack/readme.txt", ".hidden/readme.txt", "__MACOSX/readme.txt"} {
		name := []byte(template)
		for pos, original := range name {
			for value := 0; value < 256; value++ {
				name[pos] = byte(value)
				check([]pluginFile{{name: string(name)}})
			}
			name[pos] = original
		}
	}
	for _, files := range [][]pluginFile{
		{{name: "a/readme.txt"}, {name: "readme.txt"}},
		{{name: "readme.txt"}, {name: "a/readme.txt"}},
		{{name: "a/readme.txt"}, {name: "b/license.txt"}},
		{{name: "a/readme.txt"}, {name: "a/README.txt"}},
		{{name: "a", mode: fs.ModeDir | 0755}, {name: "a/readme.txt"}},
		{{name: "a/b", mode: fs.ModeDir | 0755}},
		{{name: ".hidden/link", mode: fs.ModeSymlink | 0755}},
		{{name: ".hidden/pipe", mode: fs.ModeNamedPipe | 0600}},
		{{name: ".hidden/secret.txt", flags: 1}},
	} {
		check(files)
	}
	for _, count := range []int{16, 17, 64, 65} {
		var files []pluginFile
		for i := 0; i < count; i++ {
			files = append(files, pluginFile{name: fmt.Sprintf("pack/file%02d.txt", i)})
		}
		check(files)
		files[0].name = "a/b/deep.txt"
		check(files)
		files[len(files)-1].name = ".hidden/../invalid.txt"
		check(files)
	}
}

func pluginPathScanFiles(prefix string, count int) []pluginFile {
	files := make([]pluginFile, count)
	for i := range files {
		files[i].name = fmt.Sprintf("%sfile%02d.txt", prefix, i)
	}
	return files
}

func TestPluginPathScanAllocationBudget(t *testing.T) {
	for _, prefix := range []string{"", "wrapper/"} {
		archive := pluginPathScanArchive(t, pluginPathScanFiles(prefix, 16))
		allocations := testing.AllocsPerRun(100, func() {
			members, code := readPluginArchive(archive)
			if code != "" || len(members) != 16 {
				t.Fatalf("归档结果：%d 个成员，错误码 %q", len(members), code)
			}
		})
		if allocations > 257 {
			t.Errorf("前缀 %q：分配 %.0f 次，预算 257 次", prefix, allocations)
		}
		t.Logf("前缀 %q：分配 %.0f 次", prefix, allocations)
	}
}

func BenchmarkPluginPathScan(b *testing.B) {
	for _, tc := range []struct {
		name  string
		files []pluginFile
		code  string
	}{
		{"root16", pluginPathScanFiles("", 16), ""},
		{"wrapper16", pluginPathScanFiles("wrapper/", 16), ""},
		{"hidden-depth10000", []pluginFile{{name: strings.Repeat("x/", 10000) + ".cache/readme.txt"}}, ""},
		{"rejected-depth10000", []pluginFile{{name: strings.Repeat("x/", 10000) + "readme.txt"}}, "invalid_plugin_archive"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			archive := pluginPathScanArchive(b, tc.files)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				members, code := readPluginArchive(archive)
				if code != tc.code || code == "" && len(members) != len(tc.files) && tc.name != "hidden-depth10000" {
					b.Fatal("归档结果不符", code, len(members))
				}
			}
		})
	}
}
