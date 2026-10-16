package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/engine"
)

// 收录状态来自精确查重，不把逐字注音、空结果或引擎故障当成未收录。
func TestQueryListedWord(t *testing.T) {
	for _, tc := range []struct {
		name, annotation, checked string
		annotateErr, checkErr     error
		wantErr                   error
		listed                    bool
		calls                     int
	}{
		{name: "listed", annotation: `{"code":"he'cheng'ci","word":"合成词"}`, checked: `{"listed":[true]}`, listed: true, calls: 2},
		{name: "not listed", annotation: `{"code":"he'cheng'ci","word":"合成词"}`, checked: `{"listed":[false]}`, calls: 2},
		{name: "annotation unavailable", annotateErr: engine.ErrUnavailable, wantErr: engine.ErrUnavailable, calls: 1},
		{name: "annotation invalid", annotation: `{`, wantErr: engine.ErrFailure, calls: 1},
		{name: "annotation missing code", annotation: `{"word":"合成词"}`, wantErr: engine.ErrFailure, calls: 1},
		{name: "annotation wrong word", annotation: `{"code":"he'cheng","word":"合成"}`, wantErr: engine.ErrFailure, calls: 1},
		{name: "annotation invalid code", annotation: `{"code":"合成词","word":"合成词"}`, wantErr: engine.ErrFailure, calls: 1},
		{name: "check failure", annotation: `{"code":"he'cheng'ci","word":"合成词"}`, checkErr: engine.ErrFailure, wantErr: engine.ErrFailure, calls: 2},
		{name: "check cancelled", annotation: `{"code":"he'cheng'ci","word":"合成词"}`, checkErr: context.Canceled, wantErr: context.Canceled, calls: 2},
		{name: "check malformed", annotation: `{"code":"he'cheng'ci","word":"合成词"}`, checked: `{`, wantErr: engine.ErrFailure, calls: 2},
		{name: "check empty", annotation: `{"code":"he'cheng'ci","word":"合成词"}`, checked: `{"listed":[]}`, wantErr: engine.ErrFailure, calls: 2},
		{name: "check null", annotation: `{"code":"he'cheng'ci","word":"合成词"}`, checked: `{"listed":[null]}`, wantErr: engine.ErrFailure, calls: 2},
		{name: "check extra", annotation: `{"code":"he'cheng'ci","word":"合成词"}`, checked: `{"listed":[true,false]}`, wantErr: engine.ErrFailure, calls: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			query := func(ctx context.Context, request any) (json.RawMessage, error) {
				calls++
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("两步查询必须有共同的超时预算")
				}
				raw, _ := json.Marshal(request)
				if calls == 1 {
					if string(raw) != `{"operation":"annotate","text":"合成词"}` {
						t.Fatal(string(raw))
					}
					return json.RawMessage(tc.annotation), tc.annotateErr
				}
				if string(raw) != `{"entries":[{"code":"he'cheng'ci","word":"合成词"}],"operation":"listed_pinyin_batch"}` {
					t.Fatal(string(raw))
				}
				return json.RawMessage(tc.checked), tc.checkErr
			}
			raw, err := queryListedWord(t.Context(), query, "合成词")
			if !errors.Is(err, tc.wantErr) || calls != tc.calls {
				t.Fatal(string(raw), err, calls)
			}
			if err == nil {
				var result struct {
					Code, Word string
					Listed     bool
				}
				if json.Unmarshal(raw, &result) != nil || result.Code != "he'cheng'ci" || result.Word != "合成词" || result.Listed != tc.listed {
					t.Fatal(string(raw))
				}
			}
		})
	}
}

func TestListedWordHTTPValidation(t *testing.T) {
	s := fixture(t, nil)
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"text":"合成词"}`, 503},
		{`{"text":"hecheng"}`, 400},
		{`{"text":"合成 词"}`, 400},
		{`{"text":"合成词","scheme":"wubi"}`, 400},
		{`{"text":"合成词","direction":"zh-en"}`, 400},
		{`{"text":"合成词","resources":"/private"}`, 400},
		{`{"text":"` + strings.Repeat("合", 8) + `"}`, 400},
		{`{"text":"` + strings.Repeat("合", 129) + `"}`, 400},
	} {
		w := call(s, "POST", "/v1/input/listed", tc.body)
		if w.Code != tc.status {
			t.Fatal(tc.body, w.Code, w.Body.String())
		}
	}
}

// 使用锁定的真实引擎与发布词库，从 HTTP 和 QQ 共用工具入口检查收录状态。
func TestPublishedListedWord(t *testing.T) {
	binary, resources := os.Getenv("MSIME_ENGINE_TEST_BINARY"), os.Getenv("MSIME_ENGINE_TEST_RESOURCES")
	if binary == "" || resources == "" {
		t.Skip("需要原生 Engine 及校验后的发布词库")
	}
	s := fixture(t, nil)
	s.config.Engine.Binary, s.config.Engine.Resources = binary, resources
	for _, tc := range []struct {
		word   string
		listed bool
	}{{"你好", true}, {"合成甲乙丙词", false}} {
		body, _ := json.Marshal(map[string]string{"text": tc.word})
		w := call(s, "POST", "/v1/input/listed", string(body))
		var result struct {
			Word, Code string
			Listed     *bool
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Word != tc.word || result.Code == "" || result.Listed == nil || *result.Listed != tc.listed {
			t.Fatal(tc.word, w.Code, w.Body.String())
		}
		tool := s.qqPublicTool(t.Context(), "lookup_dictionary", body)
		want := `"listed":false`
		if tc.listed {
			want = `"listed":true`
		}
		if !strings.Contains(tool, "据水杉公共数据：") || !strings.Contains(tool, "/v1/input/listed") || !strings.Contains(tool, want) {
			t.Fatal(tool)
		}
	}
}
