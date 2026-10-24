package account

import (
	"math/rand"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// 冻结原有正则与完整请求准备流程，用于兼容性和性能对照。
var previousPersonalInputCode = regexp.MustCompile(`^[a-zA-Z';]{1,256}$`)
var previousPersonalEnglishCode = regexp.MustCompile(`^[a-zA-Z]{1,64}$`)
var previousPersonalEnglishPrefix = regexp.MustCompile(`^[a-zA-Z][a-zA-Z'-]{0,63}$`)
var previousPersonalQuickCode = regexp.MustCompile(`^[a-zA-Z0-9]{1,32}$`)

func previousPreparePersonalQueryCode(w http.ResponseWriter, v PersonalQuery) (map[string]any, bool) {
	if v.Kind == "" {
		v.Kind = "pinyin"
	}
	if v.Scheme == "" {
		v.Scheme = "pinyin"
	}
	if v.Profile == "" {
		v.Profile = "xiaohe"
	}
	if v.Limit == 0 {
		v.Limit = 20
	}
	if v.Limit < 1 || v.Limit > 200 || (v.Profile != "xiaohe" && v.Profile != "ziranma" && v.Profile != "shoudao" && v.Profile != "microsoft") {
		writeError(w, 400, "invalid_dictionary_query")
		return nil, false
	}
	operation := "candidates"
	switch v.Kind {
	case "pinyin", "jianpin":
		if !previousPersonalInputCode.MatchString(v.Text) || (v.Scheme != "pinyin" && v.Scheme != "shuangpin") {
			writeError(w, 400, "invalid_input_code")
			return nil, false
		}
		if v.Kind == "jianpin" {
			operation = "jianpin"
		}
	case "wubi", "wubi98":
		// 两版五笔的编码规则相同；引擎的方案名与种类同名，`wubi98` 读 98 版码表。
		if len(v.Text) > 4 || !previousPersonalEnglishCode.MatchString(v.Text) {
			writeError(w, 400, "invalid_wubi_code")
			return nil, false
		}
		v.Scheme = v.Kind
	case "english":
		if !previousPersonalEnglishPrefix.MatchString(v.Text) {
			writeError(w, 400, "invalid_english_prefix")
			return nil, false
		}
		operation = "english"
		v.Text = strings.ToLower(v.Text)
	case "quick":
		if !previousPersonalQuickCode.MatchString(v.Text) {
			writeError(w, 400, "invalid_quick_code")
			return nil, false
		}
		operation = "quick"
	default:
		writeError(w, 400, "invalid_dictionary_kind")
		return nil, false
	}
	return map[string]any{"operation": operation, "text": v.Text, "scheme": v.Scheme, "profile": v.Profile, "limit": v.Limit}, true
}

var personalCodePreparedResult map[string]any
var personalCodePreparedOK bool

func TestPersonalQueryCodePreparationTimeBudget(t *testing.T) {
	v := PersonalQuery{Kind: "pinyin", Text: strings.Repeat("a", 256)}
	measure := func(prepare func(http.ResponseWriter, PersonalQuery) (map[string]any, bool)) testing.BenchmarkResult {
		return testing.Benchmark(func(b *testing.B) {
			for b.Loop() {
				personalCodePreparedResult, personalCodePreparedOK = prepare(nil, v)
			}
		})
	}
	original := measure(previousPreparePersonalQueryCode)
	originalResult := personalCodePreparedResult
	originalOK := personalCodePreparedOK
	current := measure(preparePersonalQuery)
	if !originalOK || !personalCodePreparedOK || !reflect.DeepEqual(originalResult, personalCodePreparedResult) {
		t.Fatal("完整请求准备结果与原实现不一致")
	}
	if current.NsPerOp()*2 > original.NsPerOp() {
		t.Fatalf("完整请求准备耗时 %d ns/op，原实现 %d ns/op，预算最多为原实现的一半", current.NsPerOp(), original.NsPerOp())
	}
}

func BenchmarkPersonalQueryCodePreparation(b *testing.B) {
	for _, tc := range []struct {
		name  string
		query PersonalQuery
	}{
		{"pinyin_short", PersonalQuery{Kind: "pinyin", Text: "nihao"}},
		{"pinyin_maximum", PersonalQuery{Kind: "pinyin", Text: strings.Repeat("a", 256)}},
		{"english_maximum", PersonalQuery{Kind: "english", Text: strings.Repeat("a", 64)}},
		{"wubi", PersonalQuery{Kind: "wubi", Text: "Abcd"}},
		{"quick_maximum", PersonalQuery{Kind: "quick", Text: strings.Repeat("a1", 16)}},
	} {
		for _, impl := range []struct {
			name    string
			prepare func(http.ResponseWriter, PersonalQuery) (map[string]any, bool)
		}{
			{"previous", previousPreparePersonalQueryCode}, {"current", preparePersonalQuery},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					personalCodePreparedResult, personalCodePreparedOK = impl.prepare(nil, tc.query)
				}
			})
		}
	}
}

func previousPersonalQueryCodeMatches(text, kind string) bool {
	switch kind {
	case "pinyin", "jianpin":
		return previousPersonalInputCode.MatchString(text)
	case "wubi", "wubi98":
		return len(text) <= 4 && previousPersonalEnglishCode.MatchString(text)
	case "english":
		return previousPersonalEnglishPrefix.MatchString(text)
	case "quick":
		return previousPersonalQuickCode.MatchString(text)
	}
	return false
}

func checkPersonalQueryCodePreparation(t *testing.T, query PersonalQuery) {
	t.Helper()
	currentWriter, previousWriter := httptest.NewRecorder(), httptest.NewRecorder()
	current, currentOK := preparePersonalQuery(currentWriter, query)
	previous, previousOK := previousPreparePersonalQueryCode(previousWriter, query)
	if currentOK != previousOK || !reflect.DeepEqual(current, previous) || currentWriter.Code != previousWriter.Code || !reflect.DeepEqual(currentWriter.Header(), previousWriter.Header()) || currentWriter.Body.String() != previousWriter.Body.String() {
		t.Fatalf("查询=%+v：结果=%v/%t/%d/%q，原结果=%v/%t/%d/%q", query, current, currentOK, currentWriter.Code, currentWriter.Body.String(), previous, previousOK, previousWriter.Code, previousWriter.Body.String())
	}
}

func checkPersonalQueryCodeScan(t *testing.T, kind, text string) {
	t.Helper()
	if got, want := validPersonalQueryCode(text, kind), previousPersonalQueryCodeMatches(text, kind); got != want {
		t.Fatalf("种类=%q 编码=%q：校验=%t，原校验=%t", kind, text, got, want)
	}
}

func TestPersonalQueryCodeScanCompatibility(t *testing.T) {
	for _, kind := range []string{"", "pinyin", "jianpin", "wubi", "wubi98", "english", "quick", "unknown"} {
		for _, text := range []string{"", "ni'hao", ";", "'", "-", "A", "AZaz", "0123", "a-b'", "a--''", "a;b", "a_b", "a.b", "a b", "a\n", "\na", "a\r\n", "a\x00", "水杉", "é", "K", "ſ", "a\xff", "\xc0\xaf", "e\u0301"} {
			checkPersonalQueryCodeScan(t, kind, text)
			checkPersonalQueryCodePreparation(t, PersonalQuery{Kind: kind, Text: text})
		}
		for _, n := range []int{0, 1, 3, 4, 5, 31, 32, 33, 63, 64, 65, 255, 256, 257, 1024} {
			for _, symbol := range []string{"a", "Z", "1", "'", ";", "-", "水", "\xff"} {
				text := strings.Repeat(symbol, n)
				checkPersonalQueryCodeScan(t, kind, text)
				checkPersonalQueryCodePreparation(t, PersonalQuery{Kind: kind, Text: text})
			}
		}
	}
}

func TestPersonalQueryCodeScanBytePairs(t *testing.T) {
	for _, kind := range []string{"pinyin", "jianpin", "wubi", "wubi98", "english", "quick"} {
		for first := range 256 {
			for second := range 256 {
				text := string([]byte{byte(first), byte(second)})
				checkPersonalQueryCodeScan(t, kind, text)
			}
		}
	}
}

func TestPersonalQueryCodePreparationCompatibility(t *testing.T) {
	for _, kind := range []string{"", "pinyin", "jianpin", "wubi", "wubi98", "english", "quick", "unknown"} {
		for _, text := range []string{"", "a", "a';", "HELLO", "a-b", "abcde", "1", "a\n", "\xff"} {
			for _, config := range []PersonalQuery{
				{}, {Scheme: "shuangpin"}, {Scheme: "unknown"}, {Profile: "ziranma"}, {Profile: "shoudao"}, {Profile: "microsoft"}, {Profile: "unknown"},
				{Limit: -1}, {Limit: 1}, {Limit: 200}, {Limit: 201}, {Scheme: "unknown", Profile: "unknown", Limit: -1},
			} {
				config.Kind, config.Text = kind, text
				checkPersonalQueryCodePreparation(t, config)
			}
		}
	}
	rng := rand.New(rand.NewSource(179))
	alphabet := []byte("abcdABCD0123';-\t\r\n_\xff水杉")
	kinds := []string{"", "pinyin", "jianpin", "wubi", "wubi98", "english", "quick", "unknown"}
	for range 1000 {
		buf := make([]byte, rng.Intn(300))
		for i := range buf {
			buf[i] = alphabet[rng.Intn(len(alphabet))]
		}
		query := PersonalQuery{Kind: kinds[rng.Intn(len(kinds))], Text: string(buf)}
		checkPersonalQueryCodeScan(t, query.Kind, query.Text)
		checkPersonalQueryCodePreparation(t, query)
	}
}

func TestPersonalQueryCodeScanAvoidsAllocations(t *testing.T) {
	for _, kind := range []string{"pinyin", "jianpin", "wubi", "wubi98", "english", "quick", "unknown"} {
		for _, text := range []string{"", "a", "a';", strings.Repeat("a", 256), strings.Repeat("a", 257), "a\xff"} {
			var got bool
			if allocations := testing.AllocsPerRun(100, func() { got = validPersonalQueryCode(text, kind) }); allocations != 0 {
				t.Fatalf("种类=%q 编码=%q：分配 %.0f 次", kind, text, allocations)
			}
			if want := previousPersonalQueryCodeMatches(text, kind); got != want {
				t.Fatalf("校验=%t，原校验=%t", got, want)
			}
		}
	}
}

func FuzzPersonalQueryCodeScanCompatibility(f *testing.F) {
	kinds := []string{"", "pinyin", "jianpin", "wubi", "wubi98", "english", "quick", "unknown"}
	for i := range len(kinds) {
		for _, text := range []string{"", "ni'hao", "HELLO", "a-b'", ";", "1", "水杉\xff", strings.Repeat("a", 256), strings.Repeat("a", 257)} {
			f.Add(uint8(i), text)
		}
	}
	f.Fuzz(func(t *testing.T, index uint8, text string) {
		kind := kinds[int(index)%len(kinds)]
		checkPersonalQueryCodeScan(t, kind, text)
		checkPersonalQueryCodePreparation(t, PersonalQuery{Kind: kind, Text: text})
	})
}

func BenchmarkPersonalQueryCodeValidation(b *testing.B) {
	for _, tc := range []struct{ name, kind, text string }{
		{"pinyin_short", "pinyin", "nihao"},
		{"pinyin_maximum", "pinyin", strings.Repeat("a", 256)},
		{"pinyin_punctuation", "pinyin", strings.Repeat("a';", 85) + "a"},
		{"english_maximum", "english", strings.Repeat("a", 64)},
		{"wubi", "wubi", "Abcd"},
		{"quick_maximum", "quick", strings.Repeat("a1", 16)},
	} {
		for _, impl := range []struct {
			name     string
			validate func(string, string) bool
		}{
			{"previous", previousPersonalQueryCodeMatches}, {"scan", validPersonalQueryCode},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					personalCodePreparedOK = impl.validate(tc.text, tc.kind)
				}
			})
		}
	}
}
