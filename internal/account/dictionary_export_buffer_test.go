package account

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func dictionaryExportFixture(tb testing.TB, count int) (*Service, Tokens) {
	tb.Helper()
	dsn := os.Getenv("MSIME_TEST_DATABASE_URL")
	if dsn == "" {
		tb.Skip("设置 MSIME_TEST_DATABASE_URL 运行 PostgreSQL 导出测试")
	}
	if !strings.Contains(dsn, "msime_auth_test") {
		tb.Fatal("仅允许使用一次性测试数据库")
	}
	db, err := Open(tb.Context(), dsn)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(db.Close)
	if err = db.Migrate(tb.Context()); err != nil {
		tb.Fatal(err)
	}
	identity := Identity{"email", "export-" + randomToken()[:8] + "@example.test"}
	challenge := Challenge{IDHash: hash(randomToken()), Provider: identity.Provider, Subject: identity.Subject}
	if err = db.PutChallenge(tb.Context(), challenge); err != nil {
		tb.Fatal(err)
	}
	user, err := db.Complete(tb.Context(), challenge, identity)
	if err != nil {
		tb.Fatal(err)
	}
	if count > 0 {
		entries := make([]DictionaryEntry, count)
		for i := range entries {
			entries[i] = DictionaryEntry{Code: fmt.Sprintf("c%04d", i), Word: fmt.Sprintf("合成词%04d", i), Weight: int64(10000 + i)}
		}
		if _, err = db.ImportDictionary(tb.Context(), user.User.ID, "quick", entries); err != nil {
			tb.Fatal(err)
		}
	}
	return &Service{store: db}, user
}

func dictionaryExportRequest(token, kind, format string) *http.Request {
	r := httptest.NewRequest("GET", "/?format="+format, nil)
	r.SetPathValue("kind", kind)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

func TestDictionaryExportBufferedContent(t *testing.T) {
	a, user := dictionaryExportFixture(t, 0)
	empty := httptest.NewRecorder()
	a.dictionaryExport(empty, dictionaryExportRequest(user.AccessToken, "quick", "standard"))
	if empty.Code != 200 || empty.Body.Len() != 0 {
		t.Fatalf("空词库导出为 %d %q", empty.Code, empty.Body.String())
	}
	entries := []DictionaryEntry{
		{Code: "a", Word: "水杉😀", Weight: 0},
		{Code: "b", Word: strings.Repeat("水", 180) + "😀", Weight: math.MaxInt64},
		{Code: "c", Word: "尾词", Weight: 12345},
	}
	for _, kind := range dictionaryKinds {
		if _, err := a.store.ImportDictionary(t.Context(), user.User.ID, kind, entries); err != nil {
			t.Fatal(err)
		}
		for _, format := range []string{"standard", "windows"} {
			t.Run(kind+"/"+format, func(t *testing.T) {
				var want strings.Builder
				for _, entry := range entries {
					first, second := entry.Word, entry.Code
					if format == "windows" && (kind == "english" || kind == "quick") {
						first, second = second, first
					}
					fmt.Fprintf(&want, "%s\t%s\t%d\n", first, second, entry.Weight)
				}
				w := httptest.NewRecorder()
				a.dictionaryExport(w, dictionaryExportRequest(user.AccessToken, kind, format))
				if w.Code != 200 || w.Body.String() != want.String() {
					t.Fatalf("导出为 %d %q，期望 %q", w.Code, w.Body.String(), want.String())
				}
				if w.Header().Get("Content-Type") != "text/plain; charset=utf-8" || w.Header().Get("Content-Disposition") != `attachment; filename="dictionary-`+kind+`.tsv"` {
					t.Fatalf("导出响应头不一致：%v", w.Header())
				}
			})
		}
	}
}

type dictionaryExportFailureWriter struct {
	*httptest.ResponseRecorder
	after, writes int
}

func (w *dictionaryExportFailureWriter) Write(p []byte) (int, error) {
	if w.writes == w.after {
		return 0, errors.New("合成响应写入失败")
	}
	w.writes++
	return w.ResponseRecorder.Write(p)
}

func TestDictionaryExportBufferedWriteFailure(t *testing.T) {
	a, user := dictionaryExportFixture(t, 3)
	for _, after := range []int{0, 1} {
		t.Run(fmt.Sprint(after), func(t *testing.T) {
			w := &dictionaryExportFailureWriter{ResponseRecorder: httptest.NewRecorder(), after: after}
			aborted := false
			func() {
				defer func() {
					if value := recover(); value != nil {
						if value != http.ErrAbortHandler {
							panic(value)
						}
						aborted = true
					}
				}()
				a.dictionaryExport(w, dictionaryExportRequest(user.AccessToken, "quick", "standard"))
			}()
			want := ""
			if after == 1 {
				want = "合成词0000\tc0000\t10000\n"
			}
			if !aborted || w.Body.String() != want || w.writes != after {
				t.Fatalf("写入失败后未按原样中断：aborted=%t，正文=%q，写入=%d", aborted, w.Body.String(), w.writes)
			}
			if err := a.store.pool.Ping(context.Background()); err != nil {
				t.Fatalf("导出中断后连接不可用：%v", err)
			}
		})
	}
}

func TestDictionaryExportAllocationBudget(t *testing.T) {
	a, user := dictionaryExportFixture(t, 1000)
	for _, tc := range []struct {
		format string
		budget float64
	}{
		{"standard", 7500},
		{"windows", 13500},
	} {
		t.Run(tc.format, func(t *testing.T) {
			r := dictionaryExportRequest(user.AccessToken, "quick", tc.format)
			var w *httptest.ResponseRecorder
			allocations := testing.AllocsPerRun(3, func() {
				w = httptest.NewRecorder()
				a.dictionaryExport(w, r)
			})
			if w.Code != 200 || strings.Count(w.Body.String(), "\n") != 1000 {
				t.Fatalf("导出未返回完整的 1000 行：%d", w.Code)
			}
			// 包含真实鉴权、数据库读取和响应；给驱动留余量，同时检出逐行格式化的三次参数装箱。
			if allocations > tc.budget {
				t.Fatalf("1000 条导出分配 %.0f 次，预算 %.0f 次", allocations, tc.budget)
			}
			t.Logf("1000 条导出：%.0f 次分配", allocations)
		})
	}
}

func BenchmarkDictionaryExport(b *testing.B) {
	a, user := dictionaryExportFixture(b, 1000)
	for _, format := range []string{"standard", "windows"} {
		b.Run(format, func(b *testing.B) {
			r := dictionaryExportRequest(user.AccessToken, "quick", format)
			b.ReportAllocs()
			for b.Loop() {
				w := httptest.NewRecorder()
				a.dictionaryExport(w, r)
				if w.Code != 200 || w.Body.Len() == 0 {
					b.Fatalf("导出失败：%d %s", w.Code, w.Body.String())
				}
			}
		})
	}
}
