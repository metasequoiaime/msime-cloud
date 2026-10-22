package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
)

func TestNativeCloudUnavailableAllocationBudget(t *testing.T) {
	s := &Server{}
	r := httptest.NewRequest(http.MethodGet, "/v1/cloud/candidates?text=nihao", nil)
	for _, limit := range []int{1, 5, contract.CloudCandidates} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			var candidates []string
			var corrected bool
			allocations := testing.AllocsPerRun(100, func() {
				candidates, corrected = s.nativeCloudCandidates(r, "nihao", limit)
			})
			if candidates == nil || len(candidates) != 0 || corrected {
				t.Fatalf("未配置引擎应返回非 nil 空候选且不纠正：%v，%v", candidates, corrected)
			}
			if allocations != 0 {
				t.Fatalf("未配置引擎时分配 %.1f 次，期望零分配", allocations)
			}
		})
	}
}

func TestNativeCloudFailedBinaryReturnsEmptyArray(t *testing.T) {
	s := &Server{}
	s.config.Engine.Binary = filepath.Join(t.TempDir(), "missing-engine")
	candidates, corrected := s.nativeCloudCandidates(httptest.NewRequest(http.MethodGet, "/", nil), "nihao", 5)
	raw, err := json.Marshal(candidates)
	if err != nil || string(raw) != "[]" || corrected {
		t.Fatalf("引擎失败应返回空数组且不纠正：%s，%v，%v", raw, corrected, err)
	}
}

func TestNativeCloudCandidateFiltering(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		want           []string
		corrected      bool
	}{
		{"empty", `{}`, []string{}, false},
		{"invalid_candidates", `{"candidates":1}`, []string{}, false},
		{"filtered", `{"normalized_segmentation":"ni hao","candidates":[{"word":" "},{"word":"hello"},{"word":"你\n好"},{"word":" 你好 "},{"word":"你好"},{"word":"您好"},{"word":"你们好"}]}`, []string{"你好", "您好"}, false},
		{"corrected", `{"normalized_segmentation":"ni hao","candidates":[{"word":"你好"}]}`, []string{"你好"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{}
			s.config.Engine.Binary = fakeEngine(t, "printf '%s\\n' '"+tc.response+"'")
			text := "nihao"
			if tc.corrected {
				text = "nihaoo"
			}
			got, corrected := s.nativeCloudCandidates(httptest.NewRequest(http.MethodGet, "/", nil), text, 2)
			if !reflect.DeepEqual(got, tc.want) || corrected != tc.corrected {
				t.Fatalf("候选过滤或纠正不兼容：%v，%v；期望 %v，%v", got, corrected, tc.want, tc.corrected)
			}
		})
	}
}

func TestCloudUnavailableNativeReturnsEmptyArray(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`["SUCCESS",[["nihao",[],[],{}]]]`))
	})
	w := call(s, http.MethodGet, "/v1/cloud/candidates?text=nihao", "")
	var body struct {
		Candidates json.RawMessage `json:"candidates"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil || string(body.Candidates) != "[]" {
		t.Fatal(w.Code, w.Body.String())
	}
}

func BenchmarkNativeCloudUnavailable(b *testing.B) {
	s := &Server{}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, limit := range []int{1, 5, contract.CloudCandidates} {
		b.Run(fmt.Sprint(limit), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, _ = s.nativeCloudCandidates(r, "nihao", limit)
			}
		})
	}
}
