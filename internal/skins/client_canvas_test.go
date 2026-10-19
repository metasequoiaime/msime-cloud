package skins

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCanvasReferencesStayOutOfCatalogJSON(t *testing.T) {
	f := loadClientCases(t, "testdata/canvas_dialect.json")
	var selected clientCase
	for _, c := range f.Cases {
		if c.Name == "all canvas image references" {
			selected = c
		}
	}
	p, err := ParseStored(f.build(t, selected))
	if err != nil {
		t.Fatal(err)
	}
	got := p.ReferencedImages()
	if strings.Join(got, ",") != "panel.png,light.apng,pin.gif" {
		t.Fatal(got)
	}
	got[0] = "changed"
	if p.ReferencedImages()[0] != "panel.png" {
		t.Fatal("调用者修改了内部资源集合")
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"canvasImages", "canvas_images", "canvas", "border_image", "decorations"} {
		if _, exists := document[key]; exists {
			t.Fatal("未协商的目录字段", key)
		}
	}
	for _, resource := range p.Resources {
		if resource.Path == "light.apng" && resource.MediaType != "image/png" {
			t.Fatal(resource)
		}
	}
}
