package account

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteCommunityPageResponse(t *testing.T) {
	type item struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	for _, field := range []string{"skins", "plugins", "items"} {
		for _, count := range []int{-1, 0, 1, 19, 20, 21, 25} {
			t.Run(fmt.Sprintf("%s/%d", field, count), func(t *testing.T) {
				var items []item
				if count >= 0 {
					items = make([]item, count)
					for i := range items {
						items[i] = item{fmt.Sprintf("entry-%02d", i), "社区条目"}
					}
				}
				w := httptest.NewRecorder()
				writeCommunityPage(w, field, items)
				if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json; charset=utf-8" || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" {
					t.Fatalf("响应状态或响应头错误：%d %v", w.Code, w.Header())
				}
				var body map[string]json.RawMessage
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if len(body) != 2 || body[field] == nil || body["has_more"] == nil {
					t.Fatalf("分页响应字段错误：%s", w.Body.String())
				}
				var more bool
				if err := json.Unmarshal(body["has_more"], &more); err != nil || more != (count > 20) {
					t.Fatalf("后续页标记错误：%s，%v", w.Body.String(), err)
				}
				if count < 0 {
					if string(body[field]) != "null" {
						t.Fatalf("nil 列表应保持 null：%s", w.Body.String())
					}
					return
				}
				var got []item
				if err := json.Unmarshal(body[field], &got); err != nil {
					t.Fatal(err)
				}
				wantCount := count
				if count > 20 {
					wantCount = 20
				}
				if got == nil || len(got) != wantCount {
					t.Fatalf("分页长度或空数组错误：%s", w.Body.String())
				}
				for i, value := range got {
					if value != items[i] {
						t.Fatalf("第 %d 条的顺序或内容错误：%+v", i, value)
					}
				}
			})
		}
	}
}
