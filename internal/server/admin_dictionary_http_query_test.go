package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

func TestAdminDictionaryHTTPQueryCompatibility(t *testing.T) {
	f := &dictionaryMock{text: strings.Repeat("甲\tjia\t1\n", 101) + "乙\tyi\t5\n"}
	s := &Server{adminGitHub: dictionaryMockClient(t, f), config: Config{Admin: AdminConfig{GitHub: AdminGitHubConfig{DictionaryRepo: dictTestRepo}}}}
	revision := strings.Repeat("c", 40)
	for _, tc := range []struct {
		name, endpoint, query, canonical, code string
	}{
		{"搜索编码键值", "search", "%71=%E7%94%B2&scope=custom", "q=甲&scope=custom", ""},
		{"搜索去除两端空格", "search", "q=++JIA++&scope=custom", "q=JIA&scope=custom", ""},
		{"搜索重复项取首值", "search", "q=JIA&q=%0A&scope=custom&scope=bad&cursor=0&cursor=-1&revision=" + revision + "&revision=bad", "q=JIA&scope=custom&revision=" + revision, ""},
		{"搜索空可选首项", "search", "q=JIA&scope=&scope=bad&cursor=&cursor=-1&revision=&revision=bad", "q=JIA", ""},
		{"搜索固定版本游标", "search", "q=JIA&cursor=1&revision=" + revision, "q=JIA&cursor=1&revision=" + revision, ""},
		{"搜索忽略损坏参数对", "search", "q=JIA&scope=%zz&cursor=1;bad=x", "q=JIA", ""},
		{"搜索未知参数保留兼容", "search", "q=JIA&extra=value", "q=JIA", ""},
		{"搜索空首项拒绝", "search", "q=&q=JIA", "", "invalid_query"},
		{"搜索校验未裁剪的原词", "search", "q=%0AJIA%0A", "", "invalid_query"},
		{"搜索非法UTF8", "search", "q=%FF", "", "invalid_query"},
		{"搜索非法范围首项", "search", "q=JIA&scope=bad&scope=custom", "", "invalid_query"},
		{"搜索非法游标首项", "search", "q=JIA&cursor=-1&cursor=0", "", "invalid_query"},
		{"搜索游标需要版本", "search", "q=JIA&cursor=1", "", "invalid_query"},
		{"搜索非法版本首项", "search", "q=JIA&revision=bad&revision=" + revision, "", "invalid_query"},
		{"文件编码键值", "file", "p%61th=custom%2Fwords.txt&p%61ge=2&%71=%E7%94%B2", "path=custom/words.txt&page=2&q=甲", ""},
		{"文件路径空格", "file", "path=++custom/words.txt++", "path=custom/words.txt", ""},
		{"文件重复项取首值", "file", "path=custom/words.txt&path=bad&page=2&page=-1&q=JIA&q=%0A", "path=custom/words.txt&page=2&q=JIA", ""},
		{"文件空可选首项", "file", "path=custom/words.txt&page=&page=-1&q=&q=%0A", "path=custom/words.txt", ""},
		{"文件忽略损坏参数对", "file", "path=custom/words.txt&page=%zz&q=JIA;bad=x", "path=custom/words.txt", ""},
		{"文件未知参数保留兼容", "file", "path=custom/words.txt&extra=value", "path=custom/words.txt", ""},
		{"文件空路径首项", "file", "path=&path=custom/words.txt", "", "invalid_dictionary_path"},
		{"文件非法路径首项", "file", "path=../custom/words.txt&path=custom/words.txt", "", "invalid_dictionary_path"},
		{"文件分页先于搜索校验", "file", "path=custom/words.txt&page=0&q=%0A", "", "invalid_page"},
		{"文件分页上限", "file", "path=custom/words.txt&page=1000001", "", "invalid_page"},
		{"文件分页整数溢出", "file", "path=custom/words.txt&page=999999999999999999999999", "", "invalid_page"},
		{"文件搜索控制字符", "file", "path=custom/words.txt&q=%0A", "", "invalid_query"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.mu.Lock()
			before := len(f.requests)
			f.mu.Unlock()
			w := dictionaryDirect(s, "GET", "/api/dictionary/"+tc.endpoint+"?"+tc.query, "", false)
			if tc.code != "" {
				if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
					t.Fatalf("错误响应不一致：%d %s", w.Code, w.Body.String())
				}
				f.mu.Lock()
				after := len(f.requests)
				f.mu.Unlock()
				if after != before {
					t.Fatal("参数无效时不应请求上游")
				}
				return
			}
			canonical := dictionaryDirect(s, "GET", "/api/dictionary/"+tc.endpoint+"?"+tc.canonical, "", false)
			if w.Code != 200 || canonical.Code != 200 || w.Body.String() != canonical.Body.String() {
				t.Fatalf("与标准查询的响应不同：%d %s / %d %s", w.Code, w.Body.String(), canonical.Code, canonical.Body.String())
			}
		})
	}
}

func adminDictionaryRejectedQueryCases() []struct {
	name, query string
	handle      func(*Server, http.ResponseWriter, *http.Request, *githubapp.Client)
} {
	return []struct {
		name, query string
		handle      func(*Server, http.ResponseWriter, *http.Request, *githubapp.Client)
	}{
		{"search", "%71=JIA&scope=invalid&cursor=0&revision=" + strings.Repeat("c", 40), (*Server).dictionarySearch},
		{"file", "p%61th=custom%2Fwords.txt&page=2&%71=%0A", (*Server).dictionaryFile},
	}
}

func TestAdminDictionaryHTTPQueryAllocationBudget(t *testing.T) {
	s := &Server{}
	for _, tc := range adminDictionaryRejectedQueryCases() {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/dictionary/"+tc.name+"?"+tc.query, nil)
			var w *httptest.ResponseRecorder
			allocations := testing.AllocsPerRun(100, func() {
				w = httptest.NewRecorder()
				tc.handle(s, w, r, nil)
			})
			if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"invalid_query"`) {
				t.Fatalf("未执行参数校验：%d %s", w.Code, w.Body.String())
			}
			// 预算包含真实错误响应，并给编码器留余量，同时检出重复的完整查询解析。
			if allocations > 40 {
				t.Fatalf("真实处理函数分配 %.0f 次，预算 40 次", allocations)
			}
			t.Logf("真实处理函数分配：%.0f 次", allocations)
		})
	}
}

func BenchmarkAdminDictionaryHTTPQuery(b *testing.B) {
	s := &Server{}
	for _, tc := range adminDictionaryRejectedQueryCases() {
		b.Run(tc.name, func(b *testing.B) {
			r := httptest.NewRequest("GET", "/api/dictionary/"+tc.name+"?"+tc.query, nil)
			b.ReportAllocs()
			for b.Loop() {
				w := httptest.NewRecorder()
				tc.handle(s, w, r, nil)
				if w.Code != 400 {
					b.Fatal(w.Code, w.Body.String())
				}
			}
		})
	}
}
