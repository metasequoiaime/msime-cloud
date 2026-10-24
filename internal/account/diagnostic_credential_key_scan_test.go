package account

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/metasequoiaime/MSIME-Backend/internal/textutil"
)

// 冻结凭据键正则、递归配置校验和完整上传校验，作为兼容性与性能对照。
var previousDiagnosticCredentialKey = regexp.MustCompile(`(?i)token|secret|password|api_key|key$`)

func previousValidateDiagnosticUpload(v diagnosticUpload) string {
	if !textutil.ValidClientLabel(v.Platform) || !textutil.ValidClientLabel(v.AppVersion) {
		return "invalid_client"
	}
	if _, ok := diagnosticTTLs[v.TTL]; !ok {
		return "invalid_ttl"
	}
	s := v.Sections
	if len(s.Names()) == 0 {
		return "empty_snapshot"
	}
	if s.CrashLogs != nil {
		if len(*s.CrashLogs) > diagnosticCrashLimit {
			return "invalid_crash_logs"
		}
		for _, c := range *s.CrashLogs {
			if _, err := time.Parse(time.RFC3339, c.At); err != nil || len(c.At) > 64 || !textutil.ValidUTF8Text(c.Message, diagnosticCrashMessageBytes) || !textutil.ValidUTF8Text(c.Stack, diagnosticCrashStackBytes) {
				return "invalid_crash_logs"
			}
		}
	}
	if s.PerfTrace != nil && !validDiagnosticEvents(*s.PerfTrace, true) {
		return "invalid_perf_trace"
	}
	if s.InputEvents != nil && !validDiagnosticEvents(*s.InputEvents, false) {
		return "invalid_input_events"
	}
	if s.ConfigSnapshot != nil {
		entries := 0
		if code := previousValidateDiagnosticConfig(s.ConfigSnapshot, 1, &entries); code != "" {
			return code
		}
	}
	return ""
}

func previousValidateDiagnosticConfig(value any, depth int, entries *int) string {
	if depth > diagnosticConfigDepth {
		return "invalid_config_snapshot"
	}
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			*entries++
			if *entries > diagnosticConfigEntries || len(key) > diagnosticConfigKeyBytes || strings.ContainsRune(key, 0) {
				return "invalid_config_snapshot"
			}
			if previousDiagnosticCredentialKey.MatchString(key) {
				if child != DiagnosticRedacted {
					return "unredacted_config"
				}
				continue
			}
			if code := previousValidateDiagnosticConfig(child, depth+1, entries); code != "" {
				return code
			}
		}
	case []any:
		for _, child := range v {
			*entries++
			if *entries > diagnosticConfigEntries {
				return "invalid_config_snapshot"
			}
			if code := previousValidateDiagnosticConfig(child, depth+1, entries); code != "" {
				return code
			}
		}
	case string:
		if len(v) > diagnosticConfigStringBytes || strings.ContainsRune(v, 0) {
			return "invalid_config_snapshot"
		}
	}
	return ""
}

var diagnosticCredentialValidationCode string

func diagnosticCredentialUploadFixture(tb testing.TB, long bool) diagnosticUpload {
	tb.Helper()
	config := make(map[string]any, diagnosticConfigEntries)
	for i := range diagnosticConfigEntries {
		key := fmt.Sprintf("setting_%04d", i)
		if long {
			key = strings.Repeat("x", diagnosticConfigKeyBytes-len(key)) + key
		}
		config[key] = "system"
	}
	v := diagnosticUpload{Platform: "windows", AppVersion: "1.0.0", TTL: "one_day", Sections: DiagnosticSections{ConfigSnapshot: config}}
	body, err := json.Marshal(v)
	if err != nil || len(body) > diagnosticsBodyBytes || previousValidateDiagnosticUpload(v) != "" {
		tb.Fatalf("诊断上传样本无效：字节数=%d，错误=%v", len(body), err)
	}
	return v
}

func TestDiagnosticCredentialUploadTimeBudget(t *testing.T) {
	v := diagnosticCredentialUploadFixture(t, true)
	measure := func(validate func(diagnosticUpload) string) testing.BenchmarkResult {
		return testing.Benchmark(func(b *testing.B) {
			for b.Loop() {
				diagnosticCredentialValidationCode = validate(v)
			}
		})
	}
	original := measure(previousValidateDiagnosticUpload)
	originalCode := diagnosticCredentialValidationCode
	current := measure(validateDiagnosticUpload)
	if originalCode != "" || diagnosticCredentialValidationCode != "" {
		t.Fatal("有效诊断上传未通过校验")
	}
	// Go 不给测试文件中的对照实现插桩，覆盖率模式下两边的计时条件不同。
	// 仍执行真实函数和结果断言；原有耗时门槛由未启用覆盖率的竞态 CI 检查。
	if mode := testing.CoverMode(); mode != "" {
		t.Logf("覆盖率模式 %s 不比较插桩条件不同的耗时：原实现 %d ns/op，当前 %d ns/op", mode, original.NsPerOp(), current.NsPerOp())
		return
	}
	if current.NsPerOp()*2 > original.NsPerOp() {
		t.Fatalf("完整诊断上传校验耗时 %d ns/op，原实现 %d ns/op，预算最多为原实现的一半", current.NsPerOp(), original.NsPerOp())
	}
}

func BenchmarkDiagnosticCredentialUploadValidation(b *testing.B) {
	for _, size := range []struct {
		name string
		long bool
	}{{"short", false}, {"maximum", true}} {
		v := diagnosticCredentialUploadFixture(b, size.long)
		for _, impl := range []struct {
			name     string
			validate func(diagnosticUpload) string
		}{
			{"previous", previousValidateDiagnosticUpload}, {"current", validateDiagnosticUpload},
		} {
			b.Run(size.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					diagnosticCredentialValidationCode = impl.validate(v)
				}
			})
		}
	}
}

func checkDiagnosticCredentialKey(t *testing.T, key string) {
	t.Helper()
	if got, want := isDiagnosticCredentialKey(key), previousDiagnosticCredentialKey.MatchString(key); got != want {
		t.Fatalf("配置键=%q：凭据匹配=%t，原匹配=%t", key, got, want)
	}
	for _, value := range []any{"system", DiagnosticRedacted, nil, 1, []any{"system"}, map[string]any{"theme": "system"}} {
		config := map[string]any{key: value}
		currentEntries, previousEntries := 0, 0
		current := validateDiagnosticConfig(config, 1, &currentEntries)
		previous := previousValidateDiagnosticConfig(config, 1, &previousEntries)
		if current != previous || currentEntries != previousEntries {
			t.Fatalf("配置键=%q 值类型=%T：校验=%q/%d，原校验=%q/%d", key, value, current, currentEntries, previous, previousEntries)
		}
	}
}

func TestDiagnosticCredentialKeySemantics(t *testing.T) {
	for _, tc := range []struct {
		key        string
		credential bool
	}{
		{"", false}, {"theme", false}, {"TOKEN", true}, {"refresh_token_value", true}, {"xsecretary", true}, {"password_hash", true}, {"myAPI_KEY_setting", true}, {"hotkey", true}, {"monkey", true}, {"key_suffix", false}, {"key\n", false}, {"key\r\n", false}, {"api_key_suffix", true}, {"api-key", true}, {"ToKen", true}, {"ſecret", true}, {"paſſword", true}, {"api_Key", true}, {"hotKey", true}, {"Key_suffix", false}, {"配置token", true}, {"token配置", true}, {"配置主题", false}, {"\xfftoken", true}, {"secret\xff", true}, {"\xffkey", true}, {"key\xff", false}, {"\xc0\xaf", false}, {"to\x00ken", false}, {"foo\npassword", true},
	} {
		if got := isDiagnosticCredentialKey(tc.key); got != tc.credential {
			t.Fatalf("配置键=%q：凭据匹配=%t，期望=%t", tc.key, got, tc.credential)
		}
		checkDiagnosticCredentialKey(t, tc.key)
	}
}

func TestDiagnosticCredentialKeyCompatibility(t *testing.T) {
	for _, word := range []string{"token", "secret", "password", "api_key", "key"} {
		for mask := range 1 << len(word) {
			mixed := []byte(word)
			for i := range mixed {
				if mask&(1<<i) != 0 && mixed[i] >= 'a' && mixed[i] <= 'z' {
					mixed[i] -= 'a' - 'A'
				}
			}
			for _, prefix := range []string{"", "prefix_", "\n", "配置", "\xff"} {
				for _, suffix := range []string{"", "_suffix", "\n", "配置", "\xff"} {
					checkDiagnosticCredentialKey(t, prefix+string(mixed)+suffix)
				}
			}
		}
		for i, r := range word {
			for folded := unicode.SimpleFold(r); folded != r; folded = unicode.SimpleFold(folded) {
				for _, suffix := range []string{"", "_suffix", "\n"} {
					checkDiagnosticCredentialKey(t, "prefix_"+word[:i]+string(folded)+word[i+1:]+suffix)
				}
			}
			for b := range 256 {
				checkDiagnosticCredentialKey(t, word[:i]+string([]byte{byte(b)})+word[i+1:])
			}
		}
	}
	for first := range 256 {
		for second := range 256 {
			key := string([]byte{byte(first), byte(second)})
			if got, want := isDiagnosticCredentialKey(key), previousDiagnosticCredentialKey.MatchString(key); got != want {
				t.Fatalf("配置键=%q：匹配=%t，原匹配=%t", key, got, want)
			}
		}
	}
	for _, n := range []int{0, 1, 127, 128, 129, 1024} {
		for _, symbol := range []string{"x", "T", "水", "\xff"} {
			checkDiagnosticCredentialKey(t, strings.Repeat(symbol, n))
			checkDiagnosticCredentialKey(t, strings.Repeat(symbol, n)+"KEY")
		}
	}
	rng := rand.New(rand.NewSource(2000))
	alphabet := []byte("tokenSECRETpasswordapi_key_- \n\x00\xff水杉Kſ")
	for range 1000 {
		key := make([]byte, rng.Intn(160))
		for i := range key {
			key[i] = alphabet[rng.Intn(len(alphabet))]
		}
		checkDiagnosticCredentialKey(t, string(key))
	}
}

func TestDiagnosticCredentialConfigBoundsCompatibility(t *testing.T) {
	values := []any{nil, "system", strings.Repeat("x", 1025), "nul\x00", map[string]any{"token": DiagnosticRedacted}, map[string]any{"password": "synthetic-value"}, []any{"system", true, 42}}
	for _, key := range []string{"setting", "key", "secret", "theme", "hotKey", "key\n", strings.Repeat("x", 128), strings.Repeat("x", 129), "nul\x00"} {
		for _, value := range values {
			for _, depth := range []int{0, 1, 7, 8, 9} {
				for _, start := range []int{0, 1998, 1999, 2000, 2001} {
					config := map[string]any{key: value}
					currentEntries, previousEntries := start, start
					current := validateDiagnosticConfig(config, depth, &currentEntries)
					previous := previousValidateDiagnosticConfig(config, depth, &previousEntries)
					if current != previous || currentEntries != previousEntries {
						t.Fatalf("键=%q 深度=%d 起始条目=%d：结果=%q/%d，原结果=%q/%d", key, depth, start, current, currentEntries, previous, previousEntries)
					}
				}
			}
		}
	}
	for _, n := range []int{0, 1999, 2000, 2001} {
		v := diagnosticCredentialUploadFixture(t, false)
		v.Sections.ConfigSnapshot = map[string]any{"items": make([]any, n)}
		if got, want := validateDiagnosticUpload(v), previousValidateDiagnosticUpload(v); got != want {
			t.Fatalf("数组长度=%d：结果=%q，原结果=%q", n, got, want)
		}
	}
}

func TestDiagnosticCredentialUploadCompatibility(t *testing.T) {
	base := diagnosticCredentialUploadFixture(t, false)
	for _, key := range []string{"theme", "token", "PaSsWoRd", "hotkey", "key\n", "hotKey", "ſecret", strings.Repeat("x", 128), strings.Repeat("x", 129), "nul\x00"} {
		for _, value := range []any{DiagnosticRedacted, "synthetic-value", nil, map[string]any{"nested": true}, []any{1, 2}} {
			v := base
			v.Sections.ConfigSnapshot = map[string]any{key: value}
			if got, want := validateDiagnosticUpload(v), previousValidateDiagnosticUpload(v); got != want {
				t.Fatalf("键=%q 值类型=%T：上传校验=%q，原校验=%q", key, value, got, want)
			}
		}
	}
	for _, long := range []bool{false, true} {
		v := diagnosticCredentialUploadFixture(t, long)
		if got, want := validateDiagnosticUpload(v), previousValidateDiagnosticUpload(v); got != want {
			t.Fatalf("完整样本：校验=%q，原校验=%q", got, want)
		}
	}
}

func TestDiagnosticCredentialASCIIKeyAvoidsAllocations(t *testing.T) {
	for _, key := range []string{"", "setting", "TOKEN", "refresh_token_value", "PASSWORD", "api_key_setting", "hotkey", "key_suffix", strings.Repeat("x", 128)} {
		var matched bool
		if allocations := testing.AllocsPerRun(100, func() { matched = isDiagnosticCredentialKey(key) }); allocations != 0 {
			t.Fatalf("键=%q：分配 %.0f 次", key, allocations)
		}
		if matched != previousDiagnosticCredentialKey.MatchString(key) {
			t.Fatalf("键=%q：结果不一致", key)
		}
	}
}

func FuzzDiagnosticCredentialKeyCompatibility(f *testing.F) {
	for _, key := range []string{"", "theme", "TOKEN", "api_key", "hotkey", "key_suffix", "hotKey", "ſecret", "toKen", "pass\xffword", "key\n", strings.Repeat("x", 128), strings.Repeat("x", 129)} {
		f.Add(key, uint8(0), uint8(1), uint16(0))
	}
	f.Fuzz(func(t *testing.T, key string, valueIndex, depth uint8, start uint16) {
		if got, want := isDiagnosticCredentialKey(key), previousDiagnosticCredentialKey.MatchString(key); got != want {
			t.Fatalf("键=%q：匹配=%t，原匹配=%t", key, got, want)
		}
		values := []any{DiagnosticRedacted, "synthetic-value", nil, 1, []any{"system"}, map[string]any{"theme": "system"}}
		config := map[string]any{key: values[int(valueIndex)%len(values)]}
		currentEntries, previousEntries := int(start)%2002, int(start)%2002
		current := validateDiagnosticConfig(config, int(depth)%12, &currentEntries)
		previous := previousValidateDiagnosticConfig(config, int(depth)%12, &previousEntries)
		if current != previous || currentEntries != previousEntries {
			t.Fatalf("键=%q：结果=%q/%d，原结果=%q/%d", key, current, currentEntries, previous, previousEntries)
		}
		upload := diagnosticUpload{Platform: "windows", AppVersion: "1.0.0", TTL: "one_day", Sections: DiagnosticSections{ConfigSnapshot: config}}
		if got, want := validateDiagnosticUpload(upload), previousValidateDiagnosticUpload(upload); got != want {
			t.Fatalf("键=%q：上传校验=%q，原校验=%q", key, got, want)
		}
	})
}
