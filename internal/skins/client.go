package skins

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"
)

// 本文件校验数据库里的皮肤包。它们用跨平台客户端与 msime-windows 统一后的清单规则（base 是全局主题或 msime-windows 内置外观），不是 Parse 给内置皮肤和 skins_root 用的旧 Windows 方言。规则逐条移植 msime 的 crates/client-core/src/skin/catalog.rs load()，连拒绝原因也相同，所以这里接受的包每个客户端都能列出；testdata/client_dialect.json 是 msime 那份共享用例表的副本（scripts/sync_client_dialect.py 同步），同时约束本文件和 scripts/candidate_skins_seed.py。

// Background is the image drawn over the candidate card, with the client's defaults (cover, opacity 1) filled in.
type Background struct {
	Image   string  `json:"image"`
	Fit     string  `json:"fit"`
	Opacity float64 `json:"opacity"`
}

// ToolbarColors are passed through verbatim; clients normalize or drop colours they cannot read.
type ToolbarColors struct {
	Background string `json:"background,omitempty"`
	Border     string `json:"border,omitempty"`
	Handle     string `json:"handle,omitempty"`
	Divider    string `json:"divider,omitempty"`
	Icon       string `json:"icon,omitempty"`
	Hover      string `json:"hover,omitempty"`
}
type Toolbar struct {
	CornerRadius *float64      `json:"corner_radius_dip,omitempty"`
	Dark         ToolbarColors `json:"dark"`
	Light        ToolbarColors `json:"light"`
}

// SkinLicense is informational metadata the manifest declares; nothing is enforced from it here.
type SkinLicense struct {
	Code   string `json:"code,omitempty"`
	Assets string `json:"assets,omitempty"`
	Source string `json:"source,omitempty"`
}

// Stored is one database package: the raw skin.toml bytes and the files beside it, without their bytes.
type Stored struct {
	ID        string
	Manifest  []byte
	Resources []StoredResource
}
type StoredResource struct {
	Path   string
	Size   int
	SHA256 string
}

// reservedThemeIDs are the client's global theme ids; an external package may not take one.
var reservedThemeIDs = []string{"system", "shuishan", "light", "paper", "night", "ink", "custom"}

// baseThemeIDs are the global themes a package may be drawn over: every reserved id except custom.
var baseThemeIDs = []string{"system", "shuishan", "light", "paper", "night", "ink"}

// windowsLookIDs 是 msime-windows 的六个内置外观，与客户端 catalog/windows_looks.rs 的 WINDOWS_LOOK_IDS 一致。清单的 base 可以写它们，服务端一律按 system 返回；外观的配色由客户端读清单时补齐，接口按清单原样返回颜色。外部皮肤也不能用这些 ID。
var windowsLookIDs = []string{"fluent", "wechat", "graphite", "willow_green", "autumn_osmanthus", "microsoft"}

// windowsDefaultsFolder 是 msime-windows 放内置外观设置清单的子目录名，外部皮肤同样不能占用。
const windowsDefaultsFolder = "default"

// clientReservedID 是客户端的 is_reserved：全局主题、msime-windows 内置外观和 default 目录。
func clientReservedID(id string) bool {
	return slices.Contains(reservedThemeIDs, id) || slices.Contains(windowsLookIDs, id) || id == windowsDefaultsFolder
}

// cssColorText 是客户端的 css_color_text：颜色值会被 msime-windows 拼进 CSS 声明，只放行颜色写法用得到的字符。
func cssColorText(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("#(),.% -/", c) >= 0) {
			return false
		}
	}
	return true
}

func invalid(reason string) error { return fmt.Errorf("%w: %s", ErrInvalid, reason) }

// clientMediaType is the client's resource_content_type: the text after the last dot, lowercased.
func clientMediaType(name string) string {
	ext := strings.ToLower(name[strings.LastIndexByte(name, '.')+1:])
	switch ext {
	case "css":
		return "text/css; charset=utf-8"
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	case "svg":
		return "image/svg+xml"
	case "ico":
		return "image/x-icon"
	case "bmp":
		return "image/bmp"
	case "avif":
		return "image/avif"
	case "woff":
		return "font/woff"
	case "woff2":
		return "font/woff2"
	case "ttf":
		return "font/ttf"
	case "otf":
		return "font/otf"
	}
	return ""
}
func clientImage(name string) bool { return strings.HasPrefix(clientMediaType(name), "image/") }

// clientSafeResource is the client's safe_resource: relative, no backslash, and every segment a non-dot ASCII identifier.
func clientSafeResource(name string, max int) bool {
	if name == "" || len(name) > max || strings.ContainsRune(name, '\\') {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
		for i := 0; i < len(part); i++ {
			c := part[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

// SafeResource exposes clientSafeResource to other packages that accept package paths from clients, so they apply exactly the client's rule.
func SafeResource(name string, max int) bool { return clientSafeResource(name, max) }

// packageFiles answers what the client learns from the filesystem: whether a relative path exists (as a file or a directory) and whether it is a regular file. skin.toml is a file of every package.
type packageFiles map[string]bool

func newPackageFiles(resources []StoredResource) packageFiles {
	files := packageFiles{"skin.toml": true}
	for _, r := range resources {
		files[r.Path] = true
		for dir := r.Path; strings.Contains(dir, "/"); {
			dir = dir[:strings.LastIndexByte(dir, '/')]
			if _, seen := files[dir]; !seen {
				files[dir] = false
			}
		}
	}
	return files
}
func (f packageFiles) exists(name string) bool { _, ok := f[name]; return ok }
func (f packageFiles) isFile(name string) bool { return f[name] }
func (f packageFiles) image(name string) bool {
	return clientSafeResource(name, 256) && clientImage(name) && f.exists(name)
}

func requiredString(table map[string]any, key string, max int) (string, error) {
	value, ok := table[key].(string)
	if !ok {
		return "", invalid(key + " must be a string")
	}
	if value == "" || len(value) > max {
		return "", invalid(key + " has invalid length")
	}
	return value, nil
}
func optionalString(table map[string]any, key string, max int) (string, bool, error) {
	if _, present := table[key]; !present {
		return "", false, nil
	}
	value, err := requiredString(table, key, max)
	return value, err == nil, err
}

// number is the client's number(): absent is 0, an integer or float is itself, anything else is NaN so the range check rejects it.
func number(value any, present bool) float64 {
	if !present {
		return 0
	}
	switch n := value.(type) {
	case int64:
		return float64(n)
	case float64:
		return n
	}
	return math.NaN()
}
func numberIn(value any, max float64) (float64, bool) {
	n := number(value, true)
	return n, !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 && n <= max
}
func enumArray(table map[string]any, key string, allowed []string) ([]string, bool) {
	items, ok := table[key].([]any)
	if !ok || len(items) == 0 {
		return nil, false
	}
	values := []string{}
	for _, item := range items {
		value, ok := item.(string)
		if !ok || !slices.Contains(allowed, value) || slices.Contains(values, value) {
			return nil, false
		}
		values = append(values, value)
	}
	return values, true
}

// ParseStored validates a database package with the client's rules and returns it in the catalog shape, with skin.toml and every stored file as resources.
func ParseStored(s Stored) (Package, error) {
	var p Package
	if !SafeID(s.ID) || clientReservedID(s.ID) {
		return p, invalid("invalid skin id")
	}
	if len(s.Manifest) > 65536 {
		return p, invalid("skin.toml is too large")
	}
	if !utf8.Valid(s.Manifest) {
		return p, invalid("skin.toml is not UTF-8")
	}
	var table map[string]any
	if toml.Unmarshal(s.Manifest, &table) != nil {
		return p, invalid("invalid TOML")
	}
	if n, ok := table["schema_version"].(int64); !ok || n != 1 {
		return p, invalid("unsupported schema_version")
	}
	var err error
	if p.ID, err = requiredString(table, "id", 64); err != nil {
		return p, err
	}
	if p.ID != s.ID {
		return p, invalid("manifest id does not match folder")
	}
	if p.Name, err = requiredString(table, "name", 80); err != nil {
		return p, err
	}
	if p.Version, err = requiredString(table, "version", 32); err != nil {
		return p, err
	}
	if p.Base, err = requiredString(table, "base", 32); err != nil {
		return p, err
	}
	if p.Author, _, err = optionalString(table, "author", 120); err != nil {
		return p, err
	}
	if p.Description, _, err = optionalString(table, "description", 500); err != nil {
		return p, err
	}
	if slices.Contains(windowsLookIDs, p.Base) {
		p.Base = "system"
	}
	if !slices.Contains(baseThemeIDs, p.Base) {
		return p, invalid("base must be system or a built-in theme")
	}
	supports, ok := table["supports"].(map[string]any)
	if !ok {
		return p, invalid("missing supports")
	}
	if p.Supports.Layouts, ok = enumArray(supports, "layouts", []string{"horizontal", "vertical"}); !ok {
		return p, invalid("invalid supports")
	}
	if p.Supports.Themes, ok = enumArray(supports, "themes", []string{"dark", "light"}); !ok {
		return p, invalid("invalid supports")
	}
	window, ok := table["candidate_window"].(map[string]any)
	if !ok {
		return p, invalid("missing candidate_window")
	}
	files := newPackageFiles(s.Resources)
	if err = readWindow(&p, window, files); err != nil {
		return p, err
	}
	if p.Toolbar, err = readToolbar(table); err != nil {
		return p, err
	}
	if p.License, err = readLicense(table); err != nil {
		return p, err
	}
	stylesheet, declared, err := optionalString(table, "toolbar_stylesheet", 128)
	if err != nil {
		return p, err
	}
	if declared && (!clientSafeResource(stylesheet, 128) || strings.Contains(stylesheet, "/") || len(stylesheet) <= 4 || !strings.HasSuffix(stylesheet, ".css") || !files.isFile(stylesheet)) {
		return p, invalid("invalid toolbar_stylesheet")
	}
	p.ToolbarStylesheet = stylesheet
	preview, declared, err := optionalString(table, "preview", 256)
	if err != nil {
		return p, err
	}
	if declared && (!clientSafeResource(preview, 256) || !files.exists(preview)) {
		return p, invalid("invalid preview")
	}
	p.Preview = preview
	if err = readColors(&p, table); err != nil {
		return p, err
	}
	p.SchemaVersion = 1
	return p, storedResources(&p, s)
}

func readWindow(p *Package, window map[string]any, files packageFiles) error {
	w := &p.CandidateWindow
	value, present := window["min_width_dip"]
	if w.MinWidth = number(value, present); !bounded(w.MinWidth, 1000) {
		return invalid("invalid min_width_dip")
	}
	if value, present := window["corner_radius_dip"]; present {
		radius, ok := numberIn(value, 32)
		if !ok {
			return invalid("invalid corner_radius_dip")
		}
		w.CornerRadius = &radius
	}
	if err := checkWindowsWindowKeys(window); err != nil {
		return err
	}
	decoration := map[string]any{}
	if value, present := window["decoration"]; present {
		table, ok := value.(map[string]any)
		if !ok {
			return invalid("invalid decoration")
		}
		decoration = table
	}
	top, topSet := decoration["top_inset_dip"]
	width, widthSet := decoration["width_dip"]
	w.Decoration = &Decoration{Top: number(top, topSet), Width: number(width, widthSet)}
	d := w.Decoration
	if !bounded(d.Top, 500) || !bounded(d.Width, 1000) || (d.Top == 0) != (d.Width == 0) {
		return invalid("invalid decoration")
	}
	image, declared, err := optionalString(decoration, "image", 256)
	if err != nil {
		return err
	}
	if declared && (d.Top == 0 || !files.image(image)) {
		return invalid("invalid decoration")
	}
	d.Image = image
	align, declared, err := optionalString(decoration, "align", 16)
	if err != nil {
		return err
	}
	switch {
	case !declared:
		d.Align = "right"
	case align == "left" || align == "center" || align == "right":
		d.Align = align
	default:
		return invalid("invalid decoration")
	}
	value, present = window["background"]
	if !present {
		return nil
	}
	table, ok := value.(map[string]any)
	if !ok {
		return invalid("invalid background")
	}
	b := &Background{Fit: "cover", Opacity: 1}
	if b.Image, ok = table["image"].(string); !ok || !files.image(b.Image) {
		return invalid("invalid background")
	}
	if value, present := table["fit"]; present {
		fit, _ := value.(string)
		if fit != "cover" && fit != "contain" && fit != "stretch" {
			return invalid("invalid background")
		}
		b.Fit = fit
	}
	if value, present := table["opacity"]; present {
		if b.Opacity, ok = numberIn(value, 1); !ok {
			return invalid("invalid background")
		}
	}
	w.Background = b
	return nil
}

// checkWindowsWindowKeys 是客户端的 check_windows_window_keys：[candidate_window] 里只有 msime-windows 会画的键，别的平台按同样的规则校验，同一个包在每处的加载结果一致。
func checkWindowsWindowKeys(window map[string]any) error {
	for _, field := range []struct {
		key string
		max float64
	}{{"border_width_dip", 4}, {"item_corner_radius_dip", 16}} {
		if value, present := window[field.key]; present {
			if _, ok := numberIn(value, field.max); !ok {
				return invalid("invalid " + field.key)
			}
		}
	}
	if value, present := window["shadow"]; present {
		if shadow, _ := value.(string); shadow != "none" && shadow != "soft" && shadow != "strong" {
			return invalid("invalid shadow")
		}
	}
	if value, present := window["font_family"]; present {
		family, ok := value.(string)
		valid := ok && family != "" && len(family) <= 64
		for i := 0; valid && i < len(family); i++ {
			c := family[i]
			valid = c >= 0x80 || (c >= 0x20 && c != 0x7f && strings.IndexByte("\"'\\,;{}<>`", c) < 0)
		}
		if !valid {
			return invalid("invalid font_family")
		}
	}
	if value, present := window["page_arrows"]; present {
		if _, ok := value.(bool); !ok {
			return invalid("invalid page_arrows")
		}
	}
	return nil
}

func readToolbar(table map[string]any) (*Toolbar, error) {
	value, present := table["toolbar"]
	if !present {
		return nil, nil
	}
	toolbar, ok := value.(map[string]any)
	if !ok {
		return nil, invalid("invalid toolbar")
	}
	t := &Toolbar{}
	if value, present := toolbar["corner_radius_dip"]; present {
		radius, ok := numberIn(value, 32)
		if !ok {
			return nil, invalid("invalid toolbar")
		}
		t.CornerRadius = &radius
	}
	// Modes and keys are checked in the client's order so the first failure gives the same reason.
	for _, mode := range []struct {
		name    string
		palette *ToolbarColors
	}{{"dark", &t.Dark}, {"light", &t.Light}} {
		value, present := toolbar[mode.name]
		if !present {
			continue
		}
		colors, ok := value.(map[string]any)
		if !ok {
			return nil, invalid("invalid toolbar")
		}
		c := mode.palette
		for _, field := range []struct {
			key    string
			target *string
		}{{"background", &c.Background}, {"border", &c.Border}, {"handle", &c.Handle}, {"divider", &c.Divider}, {"icon", &c.Icon}, {"hover", &c.Hover}} {
			value, present := colors[field.key]
			if !present {
				continue
			}
			color, ok := value.(string)
			if !ok {
				return nil, invalid("invalid toolbar")
			}
			if len(color) > 80 {
				return nil, invalid("toolbar color exceeds 80 bytes")
			}
			if !cssColorText(color) {
				return nil, invalid("invalid toolbar")
			}
			*field.target = color
		}
	}
	return t, nil
}

func readLicense(table map[string]any) (*SkinLicense, error) {
	value, present := table["license"]
	if !present {
		return nil, nil
	}
	license, ok := value.(map[string]any)
	if !ok {
		return nil, invalid("invalid license")
	}
	l := &SkinLicense{}
	var err error
	for _, field := range []struct {
		key    string
		max    int
		target *string
	}{{"code", 120, &l.Code}, {"assets", 120, &l.Assets}, {"source", 500, &l.Source}} {
		if *field.target, _, err = optionalString(license, field.key, field.max); err != nil {
			return nil, err
		}
	}
	return l, nil
}

// readColors mirrors the client's serde read of [candidate]: known keys must have their type, unknown keys are ignored. 类型都对之后，再按客户端 check_colors 的顺序逐个明暗检查颜色键与右键菜单：长度不超过 80 字节，只含 CSS 颜色字符。
func readColors(p *Package, table map[string]any) error {
	value, present := table["candidate"]
	if !present {
		return nil
	}
	candidate, ok := value.(map[string]any)
	if !ok {
		return invalid("invalid candidate colors")
	}
	for mode, colors := range map[string]*Colors{"dark": &p.Candidate.Dark, "light": &p.Candidate.Light} {
		value, present := candidate[mode]
		if !present {
			continue
		}
		palette, ok := value.(map[string]any)
		if !ok {
			return invalid("invalid candidate colors")
		}
		for key, field := range map[string]*string{"accent": &colors.Accent, "selected": &colors.Selected, "hover": &colors.Hover, "surface": &colors.Surface, "border": &colors.Border, "text": &colors.Text, "number": &colors.Number, "translation": &colors.Translation} {
			value, present := palette[key]
			if !present {
				continue
			}
			color, ok := value.(string)
			if !ok {
				return invalid("invalid candidate colors")
			}
			*field = color
		}
		if value, present := palette["show_selected_bar"]; present {
			bar, ok := value.(bool)
			if !ok {
				return invalid("invalid candidate colors")
			}
			colors.ShowSelectedBar = &bar
		}
	}
	// 客户端先反序列化两个明暗，再逐个检查颜色，所以任何一处类型错误都先于长度和字符错误。
	for _, mode := range []string{"dark", "light"} {
		palette, ok := candidate[mode].(map[string]any)
		if !ok {
			continue
		}
		if err := checkColors(palette, candidateColorKeys); err != nil {
			return err
		}
		if value, present := palette["menu"]; present {
			menu, ok := value.(map[string]any)
			if !ok {
				return invalid("invalid candidate colors")
			}
			if err := checkColors(menu, menuColorKeys); err != nil {
				return err
			}
		}
	}
	return nil
}

// candidateColorKeys 是客户端的 CANDIDATE_COLOR_KEYS：前八个各平台都画，其余是 msime-windows 的细分配色，别处只校验。
var candidateColorKeys = []string{"accent", "selected", "hover", "surface", "border", "text", "number", "translation", "candidate_text", "preedit_text", "preedit_caret", "selected_text", "selected_number", "selected_translation", "selected_bar", "preedit_background", "preedit_divider"}

// menuColorKeys 是 msime-windows 候选框右键菜单的配色键。
var menuColorKeys = []string{"background", "border", "text", "hover"}

func checkColors(table map[string]any, keys []string) error {
	for _, key := range keys {
		value, present := table[key]
		if !present {
			continue
		}
		color, ok := value.(string)
		if !ok {
			return invalid("invalid candidate colors")
		}
		if len(color) > 80 {
			return invalid("candidate color exceeds 80 bytes")
		}
		if !cssColorText(color) {
			return invalid("invalid candidate colors")
		}
	}
	return nil
}

// storedResources applies the limits the filesystem sources use (4 MiB per file, 16 MiB and 512 entries per package) and lists skin.toml first, then the stored files by path.
func storedResources(p *Package, s Stored) error {
	sum := sha256.Sum256(s.Manifest)
	p.Resources = []Resource{{"skin.toml", len(s.Manifest), hex.EncodeToString(sum[:]), mediaType("skin.toml"), "/v1/skins/" + s.ID + "/resources/skin.toml"}}
	total := len(s.Manifest)
	if len(s.Resources) >= 512 {
		return invalid("too many resources")
	}
	for _, r := range s.Resources {
		kind := clientMediaType(r.Path)
		if r.Path == "skin.toml" || kind == "" || !clientSafeResource(r.Path, 256) || r.Size < 0 || r.Size > 4<<20 || len(r.SHA256) != 64 {
			return invalid("invalid resource")
		}
		total += r.Size
		if total > 16<<20 {
			return invalid("package exceeds 16 MiB")
		}
		p.Resources = append(p.Resources, Resource{r.Path, r.Size, r.SHA256, kind, "/v1/skins/" + s.ID + "/resources/" + r.Path})
	}
	slices.SortFunc(p.Resources[1:], func(a, b Resource) int { return strings.Compare(a.Path, b.Path) })
	return nil
}
