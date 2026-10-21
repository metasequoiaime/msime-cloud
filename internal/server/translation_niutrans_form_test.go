package server

import (
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func previousNiuTransTextForm(appID, apiKey, source, target, timestamp, text string) string {
	form := url.Values{
		"from": {source}, "to": {target}, "appId": {appID}, "timestamp": {timestamp}, "srcText": {text},
	}
	form.Set("authStr", niutransAuthString(appID, apiKey, source, target, timestamp, text))
	return form.Encode()
}

func TestNiuTransFormRequestAllocationBudget(t *testing.T) {
	for _, text := range []string{"word", strings.Repeat("中", 2730)} {
		s := niuTransMemoryServer()
		r := httptest.NewRequest(http.MethodPost, "/v1/translate", nil)
		v := translationRequest{Text: text, Source: "zh", Target: "en"}
		e := niuTransMemoryEndpoint()
		var w *httptest.ResponseRecorder
		allocations := testing.AllocsPerRun(100, func() {
			w = httptest.NewRecorder()
			s.translateSingle(w, r, v, e)
		})
		if w.Code != http.StatusOK {
			t.Fatal(w.Code, w.Body.String())
		}
		if allocations > 60 {
			t.Fatalf("%d 字节文本的单条上游请求分配 %.0f 次，期望至多 60 次", len(text), allocations)
		}
	}
}

func TestNiuTransTextFormMatchesPrevious(t *testing.T) {
	cases := [][6]string{
		{},
		{"synthetic-app", "synthetic-key", "zh", "en", "1700000000000", "word"},
		{"应用 +%&=", "合成密钥", "auto", "en", "1700000000000", "水杉 😀\n +%&="},
		{"\x00app", "key\x00", "from +&=", "to +&=", "time +&=", "\xff\xfe\x00"},
		{"synthetic-app", "synthetic-key", "zh", "en", "1700000000000", strings.Repeat("中", 2730)},
	}
	rng := rand.New(rand.NewSource(137))
	for size := range 128 {
		var fields [6]string
		for i := range fields {
			data := make([]byte, size)
			_, _ = rng.Read(data)
			fields[i] = string(data)
		}
		cases = append(cases, fields)
	}
	for index, fields := range cases {
		got := niutransTextForm(fields[0], fields[1], fields[2], fields[3], fields[4], fields[5])
		want := previousNiuTransTextForm(fields[0], fields[1], fields[2], fields[3], fields[4], fields[5])
		if got != want {
			t.Fatalf("第 %d 组表单字段顺序或字节编码改变", index)
		}
	}
}

func BenchmarkNiuTransTextForm(b *testing.B) {
	for _, tc := range niuTransTextAuthInputs() {
		for _, impl := range []struct {
			name string
			form func(string, string, string, string, string, string) string
		}{{"previous", previousNiuTransTextForm}, {"current", niutransTextForm}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				var body string
				for b.Loop() {
					body = impl.form("synthetic-app", "synthetic-key", "zh", "en", "1700000000000", tc.text)
				}
				if body != previousNiuTransTextForm("synthetic-app", "synthetic-key", "zh", "en", "1700000000000", tc.text) {
					b.Fatal("表单字节改变")
				}
			})
		}
	}
}
