package server

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// 汉字反查必须通过真实注音与双拼表得到完整码，不能把候选查询失败或简称当成结果。
func TestQQPublishedShuangpinWordKeys(t *testing.T) {
	binary, resources := os.Getenv("MSIME_ENGINE_TEST_BINARY"), os.Getenv("MSIME_ENGINE_TEST_RESOURCES")
	if binary == "" || resources == "" {
		t.Skip("需要真实引擎和发布词库")
	}
	s := fixture(t, nil)
	s.config.Engine.Binary, s.config.Engine.Resources = binary, resources
	for _, tc := range []struct{ profile, code, segmented string }{
		{"xiaohe", "ceui", "ce'ui"}, {"ziranma", "ceui", "ce'ui"},
		{"shoudao", "ceei", "ce'ei"}, {"microsoft", "ceui", "ce'ui"},
	} {
		for _, operation := range []string{"candidates", "annotate"} {
			args, _ := json.Marshal(map[string]any{"operation": operation, "text": "测试", "scheme": "shuangpin", "profile": tc.profile, "schema": "xiaohe", "direction": "en-zh", "time": "2026-01-02T03:04:05Z", "timezone": "UTC"})
			result := s.qqPublicTool(t.Context(), "lookup_dictionary", args)
			var response struct {
				Data struct {
					Code, Word, Pinyin, Scheme, Profile string
					Segmented                           string `json:"segmented_code"`
				} `json:"data"`
			}
			encoded := strings.TrimPrefix(result, "据水杉公共数据：")
			if json.Unmarshal([]byte(encoded), &response) != nil || response.Data.Code != tc.code || response.Data.Segmented != tc.segmented || response.Data.Pinyin != "ce'shi" || response.Data.Word != "测试" || response.Data.Scheme != "shuangpin" || response.Data.Profile != tc.profile {
				t.Fatalf("%s/%s 反查失败：%s", tc.profile, operation, result)
			}
			direct := call(s, "POST", "/v1/input/annotate", `{"text":"测试","scheme":"shuangpin","profile":"`+tc.profile+`"}`)
			if direct.Code != 200 || !strings.Contains(direct.Body.String(), `"code":"`+tc.code+`"`) {
				t.Fatalf("%s 直接接口失败：%d %s", tc.profile, direct.Code, direct.Body.String())
			}
		}
	}
}
