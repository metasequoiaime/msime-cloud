package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/metasequoiaime/MSIME-Backend/internal/publictools"
)

// 能力问题即使不触发现查，回答模型也能看到实际工具目录；帮助提供固定的公共查询用法入口。
func TestQQBotPublicToolAwareness(t *testing.T) {
	s, fake, _ := qqServer(t)
	s.qq.config.Admins = nil
	fake.selected = `{"sections":[],"lookup":false}`
	fake.answer = `{"type":"answered","answer":"可以直接在群里查发布词库和公开插件，不用另连 MCP。"}`
	qqDeliver(t, s, "m-tool-awareness", "你可以用 MCP 查词库吗")
	for _, tool := range publictools.Definitions() {
		if !strings.Contains(fake.answerSystem, tool.Name+"："+tool.Description) {
			t.Error("回答模型没有收到已接入的工具说明", tool.Name)
		}
	}
	if !strings.Contains(fake.answerSystem, "机器人自身不连接任意外部 MCP 服务器") || !strings.Contains(fake.answerSystem, "只有实际调用成功才说已查到") || len(fake.researchTools) != 0 {
		t.Fatal(fake.answerSystem, fake.researchTools)
	}
	if !strings.Contains(fake.selectSystem, "同时要求查具体词条或公共数据时仍为 true") {
		t.Fatal(fake.selectSystem)
	}
	calls := len(fake.chatCalls)
	qqDeliver(t, s, "m-tool-help", "帮助")
	last := fake.replyTexts()[len(fake.replyTexts())-1]
	if !strings.Contains(last, "/mcp") || calls != len(fake.chatCalls) {
		t.Fatal(last, fake.chatCalls)
	}
}

func TestQQPublicToolDispatch(t *testing.T) {
	s := fixture(t, nil)
	for _, tc := range []struct{ name, args, want string }{
		{"query_public_data", `{"resource":"input_capabilities"}`, "据水杉公共数据："},
		{"lookup_dictionary", `{"text":"hecheng"}`, "public_api_status_503"},
		{"lookup_dictionary", `{"text":"合成词"}`, "public_api_status_503"},
		{"browse_input_catalog", `{"kind":"symbols"}`, "public_api_status_503"},
		{"query_public_data", `{"resource":"plugins","scope":"mine"}`, "invalid_tool_arguments"},
		{"lookup_dictionary", `{"text":"a","operation":"../users/me"}`, "invalid_tool_arguments"},
		{"query_public_data", `{"resource":"plugins"}`, "public_api_status_503"},
	} {
		var call qqResearchToolCall
		call.Function.Name, call.Function.Arguments = tc.name, tc.args
		result := s.qqResearchCall(t.Context(), call)
		if !strings.Contains(result, tc.want) || strings.Contains(result, testToken) {
			t.Fatal(tc.name, result)
		}
		if strings.Contains(tc.want, "status_") && !strings.Contains(result, "不能据此判断没有词条或作品") {
			t.Fatal(result)
		}
	}
}

func TestQQPublicDictionaryRelevantOptions(t *testing.T) {
	s := fixture(t, nil)
	for _, args := range []string{
		`{"operation":"gloss","text":"hello","direction":"invalid"}`,
		`{"operation":"romaji","text":"kana","direction":"invalid"}`,
		`{"operation":"datetime","text":"date","time":"invalid"}`,
		`{"operation":"datetime","text":"date","timezone":"invalid/zone"}`,
	} {
		if got := s.qqPublicTool(t.Context(), "lookup_dictionary", json.RawMessage(args)); !strings.Contains(got, "public_api_status_400") {
			t.Fatal("当前操作的非法选项不能被默认值掩盖：", got)
		}
	}
	const args = `{"operation":"helpcode","text":"木","schema":"xiaohe","scheme":"pinyin","profile":"xiaohe","limit":20,"direction":"en-zh","time":"2026-01-02T03:04:05Z","timezone":"UTC"}`
	if got := s.qqPublicTool(t.Context(), "lookup_dictionary", json.RawMessage(args)); !strings.Contains(got, "public_api_status_503") {
		t.Fatal("合法的工具选项应通过校验后才报告引擎不可用：", got)
	}
	if response := call(s, "POST", "/v1/input/helpcode", strings.Replace(args, `"operation":"helpcode",`, "", 1)); response.Code != 400 {
		t.Fatal("直接 HTTP 接口仍应拒绝无关选项：", response.Code)
	}
}

func TestQQResearchPublicToolsAndBudget(t *testing.T) {
	s, _, _ := qqServer(t)
	rounds := 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rounds++
		var body struct {
			Tools []struct {
				Function struct{ Name string } `json:"function"`
			} `json:"tools"`
			Messages []qqResearchMessage `json:"messages"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("无法读取模型请求")
		}
		reply := map[string]any{"role": "assistant", "content": "据水杉公共数据：能力列表已查到。"}
		finish := "stop"
		if rounds == 1 {
			names := map[string]bool{}
			for _, tool := range body.Tools {
				names[tool.Function.Name] = true
			}
			for _, tool := range publictools.Definitions() {
				if !names[tool.Name] {
					t.Error("模型没有收到公共工具", tool.Name)
				}
			}
			var calls []any
			for i := 0; i < qqResearchCalls+1; i++ {
				calls = append(calls, map[string]any{"id": fmt.Sprint(i), "type": "function", "function": map[string]any{"name": "query_public_data", "arguments": `{"resource":"input_capabilities"}`}})
			}
			reply["content"], reply["tool_calls"], finish = "", calls, "tool_calls"
		} else {
			if len(body.Tools) != 0 {
				t.Error("预算耗尽后仍提供工具")
			}
			results := 0
			for _, message := range body.Messages {
				if message.Role == "tool" {
					want := "据水杉公共数据："
					if results == qqResearchCalls {
						want = "调用次数已经用完"
					}
					if !strings.Contains(message.Content, want) || message.ToolCallID != fmt.Sprint(results) {
						t.Error(message)
					}
					results++
				}
			}
			if results != qqResearchCalls+1 {
				t.Error(results)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": reply, "finish_reason": finish}}})
	}))
	defer model.Close()
	s.config.Chat.URL = model.URL
	if result := s.qqResearch(t.Context(), "查询水杉公共能力", ""); !strings.Contains(result, "据水杉公共数据") || rounds != 2 {
		t.Fatal(result, rounds)
	}
}

func TestQQPublicResponseBound(t *testing.T) {
	r := &qqPublicResponse{header: make(http.Header)}
	r.Header().Set("Content-Type", "application/json")
	if n, err := r.Write(make([]byte, publictools.MaxResponse)); err != nil || n != publictools.MaxResponse || r.status != 200 {
		t.Fatal(n, err, r.status)
	}
	r.WriteHeader(500)
	if n, err := r.Write([]byte("x")); err == nil || n != 0 || !r.tooLarge || r.body.Len() != publictools.MaxResponse || r.status != 200 {
		t.Fatal(n, err, r)
	}
}

func TestQQPublicDataAnonymousVisibility(t *testing.T) {
	admin, schema := disposableSchema(t)
	t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
	s, err := New(Config{Auth: account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	t.Cleanup(s.CloseAccounts)
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(t.Context(), "INSERT INTO "+quoted+".auth_users(id) VALUES('synthetic-owner')"); err != nil {
		t.Fatal(err)
	}
	const publicID = "ab334455-1234-4234-8234-000000000001"
	const privateID = "ab334455-1234-4234-8234-000000000002"
	for _, tc := range []struct{ id, name, visibility string }{{publicID, "public-marker", "public"}, {privateID, "private-marker", "private"}} {
		_, err := admin.Exec(t.Context(), "INSERT INTO "+quoted+`.community_candidate_skins(id,owner_id,package_id,name,version,license_assets,manifest,preview_path,request_sha256,visibility) VALUES($1,'synthetic-owner',$1,$2,'1','MIT','synthetic','preview.png',repeat('a',64),$3)`, tc.id, tc.name, tc.visibility)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ args, want string }{
		{`{"resource":"candidate_skins"}`, "public-marker"},
		{`{"resource":"candidate_skins","id":"` + publicID + `"}`, "public-marker"},
		{`{"resource":"candidate_skins","id":"` + privateID + `"}`, "public_api_status_404"},
		{`{"resource":"community_stats"}`, "据水杉公共数据："},
	} {
		result := s.qqPublicTool(t.Context(), "query_public_data", json.RawMessage(tc.args))
		if !strings.Contains(result, tc.want) || strings.Contains(result, "private-marker") {
			t.Fatal(result)
		}
	}
}

func TestQQPublicToolsPublishedDictionary(t *testing.T) {
	binary, resources := os.Getenv("MSIME_ENGINE_TEST_BINARY"), os.Getenv("MSIME_ENGINE_TEST_RESOURCES")
	if binary == "" || resources == "" {
		t.Skip("需要原生 Engine 及校验后的发布词库")
	}
	s := fixture(t, nil)
	s.config.Engine.Binary, s.config.Engine.Resources = binary, resources
	for _, tc := range []struct{ name, args, want string }{
		{"lookup_dictionary", `{"text":"nihao","limit":5}`, "你好"},
		{"lookup_dictionary", `{"operation":"annotate","text":"重庆银行"}`, "chong'qing'yin'hang"},
		{"lookup_dictionary", `{"operation":"gloss","text":"hello"}`, "喂"},
		{"lookup_dictionary", `{"operation":"helpcode","text":"木","schema":"xiaohe"}`, "(uN)"},
		{"lookup_dictionary", `{"operation":"helpcode","text":"木","schema":"xiaohe","scheme":"pinyin","profile":"xiaohe"}`, "(uN)"},
		{"lookup_dictionary", `{"operation":"helpcode","text":"木","profile":"xiaohe"}`, "(uN)"},
		{"lookup_dictionary", `{"operation":"helpcode","text":"木","schema":"xiaohe","scheme":"pinyin","profile":"xiaohe","limit":20,"direction":"en-zh","time":"2026-01-02T03:04:05Z","timezone":"UTC"}`, "(uN)"},
		{"lookup_dictionary", `{"operation":"annotate","text":"木","schema":"xiaohe","scheme":"pinyin","profile":"xiaohe","limit":20,"direction":"en-zh","time":"2026-01-02T03:04:05Z","timezone":"UTC"}`, "mu"},
		{"lookup_dictionary", `{"operation":"gloss","text":"hello","schema":"xiaohe","scheme":"pinyin","profile":"xiaohe","limit":20,"direction":"en-zh","time":"2026-01-02T03:04:05Z","timezone":"UTC"}`, "喂"},
		{"lookup_dictionary", `{"operation":"helpcode","text":"木","schema":"lantian"}`, "(aN)"},
		{"lookup_dictionary", `{"operation":"helpcode","text":"木","schema":"ziranma"}`, "(aD)"},
		{"lookup_dictionary", `{"operation":"helpcode","text":"木","schema":"shouyou2_0"}`, "(mD)"},
		{"lookup_dictionary", `{"operation":"helpcode","text":"木","schema":"shouyouplus"}`, "(mD)"},
		{"browse_input_catalog", `{"kind":"symbols","limit":5}`, "据水杉公共数据："},
	} {
		result := s.qqPublicTool(t.Context(), tc.name, json.RawMessage(tc.args))
		if !strings.Contains(result, tc.want) || strings.Contains(result, "查询失败") {
			t.Fatal(tc.name, result)
		}
	}
}

// 各操作通过真实 HTTP 校验和引擎，模型填入其他操作的默认值不能改变查询结果。
func TestQQPublicDictionaryOperationOptions(t *testing.T) {
	binary, resources := os.Getenv("MSIME_ENGINE_TEST_BINARY"), os.Getenv("MSIME_ENGINE_TEST_RESOURCES")
	if binary == "" || resources == "" {
		t.Skip("需要原生 Engine 及校验后的发布词库")
	}
	s := fixture(t, nil)
	s.config.Engine.Binary, s.config.Engine.Resources = binary, resources
	for _, tc := range []struct{ operation, text string }{
		{"candidates", "nihao"}, {"listed", "木"}, {"jianpin", "nh"},
		{"english", "hello"}, {"japanese", "konnichiha"}, {"quick", "date"},
		{"annotate", "木"}, {"gloss", "hello"}, {"helpcode", "木"},
		{"segmentation", "nihao"}, {"emoji", "xiaolian"}, {"kaomoji", "kaixin"},
		{"unicode", "4e00"}, {"romaji", "konnichiha"}, {"convert", "合成"},
		{"datetime", "date"},
	} {
		t.Run(tc.operation, func(t *testing.T) {
			base := map[string]any{"operation": tc.operation, "text": tc.text, "limit": 20}
			switch tc.operation {
			case "candidates", "jianpin", "segmentation", "emoji", "kaomoji", "annotate":
				base["scheme"], base["profile"] = "shuangpin", "ziranma"
			case "gloss":
				base["direction"] = "en-zh"
			case "romaji":
				base["direction"] = "romaji-hiragana"
			case "helpcode":
				base["schema"] = "xiaohe"
			case "datetime":
				base["time"], base["timezone"] = "2026-01-02T03:04:05Z", "Asia/Tokyo"
			}
			args, _ := json.Marshal(base)
			want := s.qqPublicTool(t.Context(), "lookup_dictionary", args)
			if strings.Contains(want, "查询失败") {
				t.Fatal(want)
			}
			for key, value := range map[string]any{"scheme": "pinyin", "profile": "xiaohe", "schema": "lantian", "direction": "en-zh", "time": "2026-01-01T00:00:00Z", "timezone": "UTC"} {
				if _, exists := base[key]; !exists {
					base[key] = value
				}
			}
			args, _ = json.Marshal(base)
			if got := s.qqPublicTool(t.Context(), "lookup_dictionary", args); got != want {
				t.Fatalf("额外默认值改变了结果：%s；期望：%s", got, want)
			}
		})
	}
}
