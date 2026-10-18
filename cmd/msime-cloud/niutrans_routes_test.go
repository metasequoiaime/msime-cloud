package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestNiuTransMediaRoutesAndDescriptions(t *testing.T) {
	h := newHarness(t)
	code, listed, errText := h.run("", "routes", "niutrans")
	if code != 0 {
		t.Fatalf("routes: %d %s", code, errText)
	}
	var routes [][2]string
	for _, kind := range []string{"documents", "images", "voice"} {
		base := "/v1/niutrans/" + kind
		routes = append(routes, [2]string{"POST", base}, [2]string{"GET", base + "/{file_no}"}, [2]string{"PUT", base + "/{file_no}/interrupt"}, [2]string{"GET", base + "/{file_no}/download"})
	}
	routes = append(routes, [2]string{"DELETE", "/v1/niutrans/documents/{file_no}"}, [2]string{"GET", "/v1/niutrans/resources"})
	if got := len(strings.FieldsFunc(listed, func(r rune) bool { return r == '\n' })); got != len(routes) {
		t.Errorf("listed %d operations, want %d: %s", got, len(routes), listed)
	}
	for _, route := range routes {
		t.Run(route[0]+" "+route[1], func(t *testing.T) {
			found := false
			for _, line := range strings.Split(listed, "\n") {
				fields := strings.Fields(line)
				if len(fields) >= 2 && fields[0] == route[0] && fields[1] == route[1] {
					found = true
				}
			}
			if !found {
				t.Errorf("route missing from catalog: %s", listed)
			}
			concrete := strings.ReplaceAll(route[1], "{file_no}", "synthetic-file")
			code, out, errText := h.run("", "describe", route[0], concrete)
			var op operation
			if err := json.Unmarshal([]byte(out), &op); code != 0 || err != nil || op.Path != route[1] || op.Method != route[0] || op.Auth != authAny {
				t.Fatalf("describe: %d %s %s (%v)", code, out, errText, err)
			}
			var detail struct {
				Parameters []struct {
					Name     string `json:"name"`
					In       string `json:"in"`
					Required bool   `json:"required"`
					Schema   struct {
						Enum    []string `json:"enum"`
						Default string   `json:"default"`
					} `json:"schema"`
				} `json:"parameters"`
				RequestBody struct {
					Required bool `json:"required"`
					Content  map[string]struct {
						Schema struct {
							Required   []string                   `json:"required"`
							Properties map[string]json.RawMessage `json:"properties"`
						} `json:"schema"`
					} `json:"content"`
				} `json:"requestBody"`
				Responses map[string]struct {
					Content map[string]struct {
						Schema map[string]any `json:"schema"`
					} `json:"content"`
				} `json:"responses"`
			}
			if err := json.Unmarshal(op.Detail, &detail); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(route[1], "{file_no}") {
				found = false
				for _, p := range detail.Parameters {
					found = found || p.Name == "file_no" && p.In == "path" && p.Required
				}
				if !found {
					t.Error("file_no path parameter missing")
				}
			}
			if route[0] == "POST" {
				schema := detail.RequestBody.Content["multipart/form-data"].Schema
				if !detail.RequestBody.Required || len(schema.Required) != 3 || !slices.Contains(schema.Required, "file") || !slices.Contains(schema.Required, "from") || !slices.Contains(schema.Required, "to") {
					t.Errorf("required upload fields: %v", schema.Required)
				}
				for _, field := range []string{"file", "from", "to", "realmCode", "termId", "memoryId", "processingMode", "translationEngine"} {
					if len(schema.Properties[field]) == 0 {
						t.Errorf("upload field missing: %s", field)
					}
				}
			}
			if route[1] == "/v1/niutrans/resources" {
				found = false
				for _, p := range detail.Parameters {
					found = found || p.Name == "action" && p.In == "query" && p.Required
				}
				if !found {
					t.Error("required resource action missing")
				}
			}
			content := detail.Responses["200"].Content
			if strings.HasSuffix(route[1], "/download") {
				found = false
				for _, p := range detail.Parameters {
					found = found || p.Name == "type" && p.In == "query" && p.Schema.Default == "1" && slices.Equal(p.Schema.Enum, []string{"0", "1", "2", "3", "4", "5"})
				}
				if !found {
					t.Error("download type query missing or incorrect")
				}
				if content["*/*"].Schema["format"] != "binary" {
					t.Error("download must describe the upstream binary content")
				}
			} else {
				media, ok := content["application/json"]
				if !ok || media.Schema["type"] != "object" {
					t.Fatal("小牛 JSON 成功响应必须是对象")
				}
				var schema struct {
					Required   []string `json:"required"`
					Properties struct {
						Code struct {
							Type string `json:"type"`
							Enum []int  `json:"enum"`
						} `json:"code"`
					} `json:"properties"`
				}
				encoded, err := json.Marshal(media.Schema)
				if err != nil || json.Unmarshal(encoded, &schema) != nil || !slices.Contains(schema.Required, "code") || schema.Properties.Code.Type != "integer" || !slices.Equal(schema.Properties.Code.Enum, []int{200}) {
					t.Error("小牛 JSON 成功响应必须要求整数 code 为 200")
				}
			}
		})
	}
}
