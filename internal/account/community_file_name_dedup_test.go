package account

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// 固定原有文件名规则，独立验证保留名判断不依赖新实现。
func previousPluginDedupFileName(name string) bool {
	if name == "" || len(name) > 64 || !isASCIIAlnum(name[0]) || name == "." || name == ".." {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(isASCIIAlnum(c) || c == '.' || c == '-' || c == '_') {
			return false
		}
	}
	stem, _, _ := strings.Cut(strings.ToLower(name), ".")
	switch stem {
	case "con", "prn", "aux", "nul":
		return false
	}
	if len(stem) == 4 && (strings.HasPrefix(stem, "com") || strings.HasPrefix(stem, "lpt")) && stem[3] >= '0' && stem[3] <= '9' {
		return false
	}
	return true
}

func TestFileNameDedupCompatibility(t *testing.T) {
	check := func(name string) {
		t.Helper()
		if got, want := validPluginFileName(name), previousPluginDedupFileName(name); got != want {
			t.Fatalf("文件名 %q：%t，期望 %t", name, got, want)
		}
	}
	stems := []string{"con", "prn", "aux", "nul", "audio", "conx", "lpt", "com", "K", "ſ", "İ", "\xff", ""}
	for i := 0; i <= 10; i++ {
		stems = append(stems, fmt.Sprintf("com%d", i), fmt.Sprintf("lpt%d", i))
	}
	for _, stem := range stems {
		for _, suffix := range []string{"", ".txt", ".TXT", "..txt", " .txt", "x", ".wav"} {
			check(stem + suffix)
			check(strings.ToUpper(stem) + suffix)
			for bits := 0; bits < 1<<min(len(stem), 5); bits++ {
				variant := []byte(stem)
				for i, c := range variant {
					if i < 5 && bits&(1<<i) != 0 && c >= 'a' && c <= 'z' {
						variant[i] = c - 'a' + 'A'
					}
				}
				check(string(variant) + suffix)
			}
		}
	}
	for _, length := range []int{1, 2, 3, 4, 63, 64, 65, 1000} {
		check(strings.Repeat("A", length))
	}
	for _, template := range []string{"Audio_01.txt", "CON.txt", "LPT9.txt"} {
		name := []byte(template)
		for pos, original := range name {
			for value := 0; value < 256; value++ {
				name[pos] = byte(value)
				check(string(name))
			}
			name[pos] = original
		}
	}
}

type fileNameDedupFixture struct {
	paths    [2]map[string][]byte
	names    [2][]string
	archives [2][]byte
	dirs     [2]string
}

func newFileNameDedupFixture(tb testing.TB) fileNameDedupFixture {
	tb.Helper()
	var fixture fileNameDedupFixture
	root := tb.TempDir()
	for variant, kind := range []string{"lower", "upper"} {
		fixture.paths[variant] = map[string][]byte{}
		contents := map[string][]byte{}
		fixture.dirs[variant] = filepath.Join(root, kind)
		if err := os.Mkdir(fixture.dirs[variant], 0700); err != nil {
			tb.Fatal(err)
		}
		for i := 0; i < maxPluginPackFiles; i++ {
			stem := fmt.Sprintf("sample_%02d", i)
			if variant == 1 {
				stem = strings.ToUpper(stem)
			}
			name := stem + ".txt"
			data := []byte("synthetic fixture")
			fixture.names[variant] = append(fixture.names[variant], name)
			contents[name] = data
			if err := os.WriteFile(filepath.Join(fixture.dirs[variant], name), data, 0600); err != nil {
				tb.Fatal(err)
			}
			if i < maxCandidateFiles {
				fixture.paths[variant]["assets/"+stem+".png"] = data
			}
		}
		archive, err := pluginSeedArchive(fixture.names[variant], contents)
		if err != nil {
			tb.Fatal(err)
		}
		fixture.archives[variant] = archive
	}
	return fixture
}

func (fixture fileNameDedupFixture) run(tb testing.TB, kind string, variant int) {
	switch kind {
	case "candidate":
		if code := validCandidateResourceFiles("", fixture.paths[variant], true); code != "" {
			tb.Fatal(code)
		}
	case "archive":
		members, code := readPluginArchive(fixture.archives[variant])
		if code != "" || len(members) != maxPluginPackFiles {
			tb.Fatalf("归档结果：%d 个文件，错误码 %q", len(members), code)
		}
		for _, name := range fixture.names[variant] {
			if member, ok := members[name]; !ok || member.size != len("synthetic fixture") || string(member.head) != "synthetic fixture" {
				tb.Fatalf("归档未保留原文件名或内容：%q", name)
			}
		}
	case "seed":
		names, contents, err := collectPluginSeedFiles(fixture.dirs[variant])
		if err != nil || !slices.Equal(names, fixture.names[variant]) || len(contents) != maxPluginPackFiles {
			tb.Fatalf("目录结果：%q，%v", names, err)
		}
		for _, name := range names {
			if string(contents[name]) != "synthetic fixture" {
				tb.Fatalf("目录未保留文件内容：%q", name)
			}
		}
	}
}

func TestFileNameDedupAllocationBudget(t *testing.T) {
	t.Run("name", func(t *testing.T) {
		valid := false
		allocations := testing.AllocsPerRun(100, func() {
			valid = validPluginFileName("SYNTHETIC_AUDIO_00.txt")
		})
		if !valid || allocations != 0 {
			t.Fatalf("文件名校验有效=%t，分配 %.0f 次，期望 0 次", valid, allocations)
		}
	})
	fixture := newFileNameDedupFixture(t)
	for _, tc := range []struct {
		kind   string
		budget float64
	}{{"candidate", float64(len(fixture.paths[1]))}, {"archive", 24}, {"seed", 24}} {
		t.Run(tc.kind, func(t *testing.T) {
			lower := testing.AllocsPerRun(100, func() { fixture.run(t, tc.kind, 0) })
			upper := testing.AllocsPerRun(100, func() { fixture.run(t, tc.kind, 1) })
			// 候选图片每个大写文件名最多增加一次小写副本分配，预算随实际资源数变化；
			// 插件场景为标准库缓冲池和目录读取保留余量。
			if delta := upper - lower; delta > tc.budget {
				t.Fatalf("大写名称额外分配 %.0f 次，小写 %.0f，大写 %.0f，预算 %.0f", delta, lower, upper, tc.budget)
			}
			t.Logf("小写 %.0f 次，大写 %.0f 次，增量 %.0f 次", lower, upper, upper-lower)
		})
	}
}

func TestFileNameDedupRejectsCaseCollisions(t *testing.T) {
	for _, tc := range []struct {
		files map[string][]byte
		code  string
	}{
		{map[string][]byte{"A.png": {1}, "a.png": {1}}, "candidate_skin_file_path"},
		{map[string][]byte{"A.png": {}, "a.png": {1}}, "candidate_skin_too_large"},
		{map[string][]byte{"A.PNG": {1}}, ""},
		{map[string][]byte{"A.gif": {1}}, "candidate_skin_file_type"},
		{map[string][]byte{"../A.png": {1}}, "candidate_skin_file_path"},
	} {
		if got := validCandidateFiles("", tc.files); got != tc.code {
			t.Fatalf("皮肤去重错误码 %q，期望 %q", got, tc.code)
		}
	}
	for _, names := range [][]string{{"SAMPLE.txt", "sample.txt"}, {"outer/SAMPLE.txt", "outer/sample.txt"}, {"CON.txt"}, {"K.txt"}} {
		contents := map[string][]byte{}
		for _, name := range names {
			contents[name] = []byte("synthetic fixture")
		}
		archive, err := pluginSeedArchive(names, contents)
		if err != nil {
			t.Fatal(err)
		}
		if _, code := readPluginArchive(archive); code != "invalid_plugin_archive" {
			t.Fatalf("归档 %q 的错误码 %q，期望 invalid_plugin_archive", names, code)
		}
	}
}

func BenchmarkFileNameDedup(b *testing.B) {
	fixture := newFileNameDedupFixture(b)
	for _, kind := range []string{"candidate", "archive", "seed"} {
		for variant, label := range []string{"lower", "upper"} {
			b.Run(kind+"/"+label, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					fixture.run(b, kind, variant)
				}
			})
		}
	}
	b.Run("name/upper", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if !validPluginFileName("SYNTHETIC_AUDIO_00.txt") {
				b.Fatal("文件名被拒绝")
			}
		}
	})
}
