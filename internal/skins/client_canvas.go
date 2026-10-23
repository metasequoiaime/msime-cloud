package skins

import (
	"math"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var clientLengthNumber = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

// 以下校验来自 msime 的共享 catalog；服务端保留原始清单，不重新解释绘制结果。
func clientOptionalBool(table map[string]any, key string) error {
	if value, present := table[key]; present {
		if _, ok := value.(bool); !ok {
			return invalid("invalid " + key)
		}
	}
	return nil
}

func clientBox(table map[string]any, key string, required bool) error {
	value, present := table[key]
	if !present {
		if required {
			return invalid("invalid border_image " + key)
		}
		return nil
	}
	values, ok := value.([]any)
	if !ok || len(values) != 1 && len(values) != 2 && len(values) != 4 {
		return invalid("invalid " + key)
	}
	for _, value := range values {
		if _, ok := numberIn(value, 500); !ok {
			return invalid("invalid " + key)
		}
	}
	return nil
}

func clientGeometry(table map[string]any) error {
	if err := clientBox(table, "preedit_margin_dip", false); err != nil {
		return err
	}
	for _, field := range []struct {
		key     string
		dip, em float64
	}{{"padding", 64, 4}, {"item_gap", 64, 4}, {"item_extra_height", 64, 4}, {"number_inset", 64, 4},
		{"item_padding_right", 64, 4}, {"item_min_width", 500, 40}, {"number_gap", 64, 4}, {"selected_bar_width", 16, 1}} {
		if value, present := table[field.key]; present {
			if text, ok := value.(string); ok {
				parsed, err := strconv.ParseFloat(strings.TrimSuffix(text, "em"), 64)
				if !strings.HasSuffix(text, "em") || !clientLengthNumber.MatchString(strings.TrimSuffix(text, "em")) || err != nil || !bounded(parsed, field.em) {
					return invalid("invalid " + field.key)
				}
			} else if _, ok := numberIn(value, field.dip); !ok {
				return invalid("invalid " + field.key)
			}
		}
	}
	for _, key := range []string{"number_scale", "selected_bar_height_em"} {
		if value, present := table[key]; present {
			if _, ok := numberIn(value, 2); !ok {
				return invalid("invalid " + key)
			}
		}
	}
	for _, key := range []string{"selected_bar_inside", "item_corners_follow_card", "justify_rows", "trim_hidden_preedit_gap"} {
		if err := clientOptionalBool(table, key); err != nil {
			return err
		}
	}
	if value, present := table["page_arrow_style"]; present && value != "chevron" && value != "triangle" {
		return invalid("invalid page_arrow_style")
	}
	return nil
}

func clientWindowGeometry(window map[string]any, toolbar any) error {
	if value, present := window["backdrop"]; present && value != "none" && value != "acrylic" && value != "mica" {
		return invalid("invalid backdrop")
	}
	for _, key := range []string{"fit_content", "expand_panel"} {
		if err := clientOptionalBool(window, key); err != nil {
			return err
		}
	}
	if value, present := window["expand_rows"]; present {
		if rows, ok := value.(int64); !ok || rows < 2 || rows > 8 {
			return invalid("invalid expand_rows")
		}
	}
	if err := clientGeometry(window); err != nil {
		return err
	}
	for _, key := range []string{"horizontal", "vertical"} {
		if value, present := window[key]; present {
			table, ok := value.(map[string]any)
			if !ok {
				return invalid("invalid " + key + " geometry")
			}
			if err := clientGeometry(table); err != nil {
				return err
			}
		}
	}
	if toolbar != nil {
		table, ok := toolbar.(map[string]any)
		if !ok {
			return invalid("invalid toolbar")
		}
		if value, present := table["icon_corner_radius_dip"]; present {
			if _, ok := numberIn(value, 16); !ok {
				return invalid("invalid icon_corner_radius_dip")
			}
		}
	}
	return nil
}

func clientAnimatedImage(files packageFiles, value string) bool {
	return slices.Contains([]string{".png", ".apng", ".gif", ".webp"}, strings.ToLower(path.Ext(value))) && files.image(value)
}

func readCanvas(p *Package, window map[string]any, files packageFiles) error {
	if value, present := window["border_image"]; present {
		table, ok := value.(map[string]any)
		if !ok {
			return invalid("invalid border_image")
		}
		source, err := requiredString(table, "source", 256)
		if err != nil {
			return err
		}
		if !clientAnimatedImage(files, source) {
			return invalid("invalid border_image source")
		}
		p.canvasImages = append(p.canvasImages, source)
		light, declared, err := optionalString(table, "light_source", 256)
		if err != nil {
			return err
		}
		if declared {
			if !clientAnimatedImage(files, light) {
				return invalid("invalid border_image light_source")
			}
			p.canvasImages = append(p.canvasImages, light)
		}
		for _, key := range []string{"slice_dip", "stretch_dip", "width_dip"} {
			if err := clientBox(table, key, key == "slice_dip"); err != nil {
				return err
			}
		}
		if err := clientOptionalBool(table, "fill"); err != nil {
			return err
		}
	}
	for _, field := range []struct {
		key                  string
		vertical, horizontal float64
	}{
		{"content_insets_dip", 200, 500}, {"outer_insets_dip", 500, 500}} {
		if value, present := window[field.key]; present {
			table, ok := value.(map[string]any)
			if !ok {
				return invalid("invalid " + field.key)
			}
			for _, side := range []string{"top", "right", "bottom", "left"} {
				limit := field.vertical
				if side == "right" || side == "left" {
					limit = field.horizontal
				}
				if value, present := table[side]; present {
					if _, ok := numberIn(value, limit); !ok {
						return invalid("invalid " + field.key)
					}
				}
			}
		}
	}
	if value, present := window["line_gap_dip"]; present {
		if _, ok := numberIn(value, 100); !ok {
			return invalid("invalid line_gap_dip")
		}
	}
	value, present := window["decorations"]
	if !present {
		return nil
	}
	decorations, ok := value.([]any)
	if !ok {
		return invalid("invalid decorations")
	}
	if len(decorations) > 32 {
		return invalid("too many decorations")
	}
	for _, value := range decorations {
		table, ok := value.(map[string]any)
		if !ok {
			return invalid("invalid decoration")
		}
		name, err := requiredString(table, "image", 256)
		if err != nil {
			return err
		}
		if !clientAnimatedImage(files, name) {
			return invalid("invalid decoration image")
		}
		if anchor, present := table["anchor"]; present {
			text, ok := anchor.(string)
			if !ok || !slices.Contains([]string{"top-left", "top-right", "bottom-left", "bottom-right", "left", "right", "top", "bottom", "center"}, text) {
				return invalid("invalid decoration anchor")
			}
		}
		for _, key := range []string{"offset_x_dip", "offset_y_dip"} {
			if value, present := table[key]; present {
				n := number(value, true)
				if math.IsNaN(n) || math.IsInf(n, 0) || n < -2000 || n > 2000 {
					return invalid("invalid decoration " + key)
				}
			}
		}
		if value, present := table["z_index"]; present {
			if z, ok := value.(int64); !ok || z < -1000 || z > 1000 {
				return invalid("invalid decoration z_index")
			}
		}
		for _, key := range []string{"width_dip", "height_dip"} {
			value, present := table[key]
			if !present {
				return invalid("missing decoration " + key)
			}
			if n, ok := numberIn(value, 2000); !ok || n == 0 {
				return invalid("invalid decoration " + key)
			}
		}
		p.canvasImages = append(p.canvasImages, name)
	}
	return nil
}

// ReferencedImages 返回生效的 canvas 图片路径，不把新字段加入既有目录 JSON。
func (p Package) ReferencedImages() []string { return slices.Clone(p.canvasImages) }
