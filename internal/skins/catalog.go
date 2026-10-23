// Package skins exposes the Windows skin manifest as portable metadata and package assets.
package skins

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/boundedio"
	checksum "github.com/metasequoiaime/MSIME-Backend/internal/digest"
	"github.com/metasequoiaime/MSIME-Backend/internal/packageid"
	"github.com/pelletier/go-toml/v2"
)

//go:embed builtin
var builtin embed.FS
var ErrNotFound = errors.New("skin_not_found")
var ErrInvalid = errors.New("invalid_skin")
var builtinIDs = []string{"fluent", "wechat", "graphite", "willow_green"}

// 内置资源随二进制固定，每款皮肤按需加载一次元数据，返回时再复制可变切片。
var builtinLoads = func() map[string]func() (Package, error) {
	loads := make(map[string]func() (Package, error), len(builtinIDs))
	for _, id := range builtinIDs {
		loads[id] = sync.OnceValues(func() (Package, error) { return loadPackage("", id) })
	}
	return loads
}()

// SafeID 额外拒绝 Windows 无法按原名安装的尾点目录与 nul 设备目录。
func SafeID(id string) bool {
	return packageid.Valid(id)
}
func Builtin(id string) bool { return slices.Contains(builtinIDs, id) }

type Colors struct {
	Accent   string `toml:"accent" json:"accent,omitempty"`
	Selected string `toml:"selected" json:"selected,omitempty"`
	Hover    string `toml:"hover" json:"hover,omitempty"`
	Surface  string `toml:"surface" json:"surface,omitempty"`
	Border   string `toml:"border" json:"border,omitempty"`
	Text     string `toml:"text" json:"text,omitempty"`
	Number   string `toml:"number" json:"number,omitempty"`
	// Translation and the other fields tagged toml:"-" below exist only in the client dialect; the Windows parser leaves them empty so builtin and skins_root responses keep their shape.
	Translation     string `toml:"-" json:"translation,omitempty"`
	ShowSelectedBar *bool  `toml:"show_selected_bar" json:"show_selected_bar,omitempty"`
}
type Package struct {
	canvasImages      []string
	SchemaVersion     int    `toml:"schema_version" json:"schema_version"`
	ID                string `toml:"id" json:"id"`
	Name              string `toml:"name" json:"name"`
	Version           string `toml:"version" json:"version"`
	Author            string `toml:"author" json:"author,omitempty"`
	Description       string `toml:"description" json:"description,omitempty"`
	Base              string `toml:"base" json:"base"`
	ToolbarStylesheet string `toml:"toolbar_stylesheet" json:"toolbar_stylesheet,omitempty"`
	Preview           string `toml:"preview" json:"preview,omitempty"`
	Supports          struct {
		Layouts []string `toml:"layouts" json:"layouts"`
		Themes  []string `toml:"themes" json:"themes"`
	} `toml:"supports" json:"supports"`
	CandidateWindow struct {
		MinWidth     float64     `toml:"min_width_dip" json:"min_width_dip"`
		CornerRadius *float64    `toml:"-" json:"corner_radius_dip,omitempty"`
		Decoration   *Decoration `toml:"decoration" json:"decoration"`
		Background   *Background `toml:"-" json:"background,omitempty"`
	} `toml:"candidate_window" json:"candidate_window"`
	Candidate struct {
		Dark  Colors `toml:"dark" json:"dark"`
		Light Colors `toml:"light" json:"light"`
	} `toml:"candidate" json:"candidate"`
	Toolbar   *Toolbar     `toml:"-" json:"toolbar,omitempty"`
	License   *SkinLicense `toml:"-" json:"license,omitempty"`
	Builtin   bool         `toml:"-" json:"builtin"`
	Resources []Resource   `toml:"-" json:"resources,omitempty"`
}
type Decoration struct {
	Top   float64 `toml:"top_inset_dip" json:"top_inset_dip"`
	Width float64 `toml:"width_dip" json:"width_dip"`
	Image string  `toml:"-" json:"image,omitempty"`
	Align string  `toml:"-" json:"align,omitempty"`
}
type Resource struct {
	Path      string `json:"path"`
	Size      int    `json:"size"`
	SHA256    string `json:"sha256"`
	MediaType string `json:"media_type"`
	URL       string `json:"url"`
}

func validText(s string, max int, required bool) bool {
	return utf8.ValidString(s) && len(s) <= max && (!required || s != "") && !strings.ContainsRune(s, 0)
}
func validList(values, allowed []string) bool {
	seen := map[string]bool{}
	if len(values) == 0 {
		return false
	}
	for _, v := range values {
		if seen[v] || !slices.Contains(allowed, v) {
			return false
		}
		seen[v] = true
	}
	return true
}
func bounded(v, max float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= max }
func safeResource(name string) bool {
	return clientSafeResource(name, 256) && mediaType(name) != ""
}
func mediaType(name string) string {
	if name == "skin.toml" {
		return "application/toml"
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".css":
		return "text/css; charset=utf-8"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	}
	return ""
}
func Parse(raw []byte, id string) (Package, error) {
	var p Package
	if len(raw) > 65536 || !SafeID(id) || Builtin(id) || toml.Unmarshal(raw, &p) != nil {
		return p, ErrInvalid
	}
	if p.SchemaVersion != 1 || p.ID != id || !Builtin(p.Base) || !validText(p.Name, 80, true) || !validText(p.Version, 32, true) || !validText(p.Author, 120, false) || !validText(p.Description, 500, false) || !validList(p.Supports.Layouts, []string{"horizontal", "vertical"}) || !validList(p.Supports.Themes, []string{"dark", "light"}) {
		return p, ErrInvalid
	}
	if p.ToolbarStylesheet != "" && (!safeResource(p.ToolbarStylesheet) || path.Base(p.ToolbarStylesheet) != p.ToolbarStylesheet || !strings.HasSuffix(p.ToolbarStylesheet, ".css") || len(p.ToolbarStylesheet) > 128) {
		return p, ErrInvalid
	}
	if p.Preview != "" && !safeResource(p.Preview) {
		return p, ErrInvalid
	}
	d := p.CandidateWindow.Decoration
	if d == nil || !bounded(p.CandidateWindow.MinWidth, 1000) || !bounded(d.Top, 500) || !bounded(d.Width, 1000) || ((d.Top == 0) != (d.Width == 0)) {
		return p, ErrInvalid
	}
	for _, c := range []Colors{p.Candidate.Dark, p.Candidate.Light} {
		for _, v := range []string{c.Accent, c.Selected, c.Hover, c.Surface, c.Border, c.Text, c.Number} {
			if !validText(v, 80, false) {
				return p, ErrInvalid
			}
		}
	}
	p.Builtin = false
	p.Resources = nil
	return p, nil
}
func builtInPackage(id string) Package {
	p := Package{SchemaVersion: 1, ID: id, Name: map[string]string{"fluent": "Fluent 主题", "wechat": "微信绿主题", "graphite": "石墨 Graphite", "willow_green": "杨柳青 Willow green"}[id], Version: "windows-" + sourceCommit[:7], Base: id, Builtin: true}
	p.Supports.Layouts = []string{"horizontal", "vertical"}
	p.Supports.Themes = []string{"dark", "light"}
	return p
}
func (p Package) Matches(layout, theme string) bool {
	return (layout == "" || slices.Contains(p.Supports.Layouts, layout)) && (theme == "" || slices.Contains(p.Supports.Themes, theme))
}
func openPackage(root, id string) (*os.Root, error) {
	if root == "" || !SafeID(id) {
		return nil, ErrNotFound
	}
	parent, err := os.OpenRoot(root)
	if err != nil {
		return nil, ErrNotFound
	}
	defer parent.Close()
	child, err := parent.OpenRoot(id)
	if err != nil {
		return nil, ErrNotFound
	}
	return child, nil
}
func readFile(fsys fs.FS, name string) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, ErrNotFound
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return nil, ErrInvalid
	}
	raw, err := boundedio.ReadAll(f, info.Size(), 4<<20)
	if err != nil || len(raw) > 4<<20 {
		return nil, ErrInvalid
	}
	return raw, nil
}
func Files(root, id string) (fs.FS, func(), error) {
	if !SafeID(id) {
		return nil, func() {}, ErrNotFound
	}
	if Builtin(id) {
		sub, err := fs.Sub(builtin, "builtin/"+id)
		return sub, func() {}, err
	}
	child, err := openPackage(root, id)
	if err != nil {
		return nil, func() {}, err
	}
	return child.FS(), func() { child.Close() }, nil
}
func Load(root, id string) (Package, error) {
	if load, ok := builtinLoads[id]; ok {
		p, err := load()
		p.Supports.Layouts = slices.Clone(p.Supports.Layouts)
		p.Supports.Themes = slices.Clone(p.Supports.Themes)
		p.Resources = slices.Clone(p.Resources)
		return p, err
	}
	return loadPackage(root, id)
}

func loadPackage(root, id string) (Package, error) {
	files, close, err := Files(root, id)
	if err != nil {
		return Package{}, err
	}
	defer close()
	var p Package
	if Builtin(id) {
		p = builtInPackage(id)
	} else {
		raw, err := readFile(files, "skin.toml")
		if err != nil {
			return p, err
		}
		p, err = Parse(raw, id)
		if err != nil {
			return p, err
		}
	}
	total, count := 0, 0
	err = fs.WalkDir(files, ".", func(name string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return ErrInvalid
		}
		count++
		if count > 512 {
			return ErrInvalid
		}
		if d.IsDir() {
			if strings.Count(name, "/") > 8 {
				return fs.SkipDir
			}
			return nil
		}
		if !safeResource(name) {
			return nil
		}
		raw, err := readFile(files, name)
		if err != nil {
			return err
		}
		total += len(raw)
		if total > 16<<20 {
			return ErrInvalid
		}
		p.Resources = append(p.Resources, Resource{name, len(raw), checksum.SHA256(raw), mediaType(name), "/v1/skins/" + id + "/resources/" + name})
		return nil
	})
	if err != nil {
		return p, err
	}
	for _, required := range []string{p.ToolbarStylesheet, p.Preview} {
		if required != "" && !slices.ContainsFunc(p.Resources, func(r Resource) bool { return r.Path == required }) {
			return p, ErrInvalid
		}
	}
	return p, nil
}

// rootIDs lists the package directories under skins_root that the catalog considers.
func rootIDs(root string) ([]string, error) {
	if root == "" {
		return nil, nil
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(dir.FS(), ".")
	dir.Close()
	if err != nil {
		return nil, err
	}
	if len(entries) > 256 {
		return nil, ErrInvalid
	}
	ids := []string{}
	for _, d := range entries {
		if d.IsDir() && SafeID(d.Name()) && !Builtin(d.Name()) {
			ids = append(ids, d.Name())
		}
	}
	return ids, nil
}
func Catalog(root, layout, theme string) ([]Package, int, error) {
	found, err := rootIDs(root)
	if err != nil {
		return nil, 0, err
	}
	ids := append(slices.Clone(builtinIDs), found...)
	slices.Sort(ids)
	packages := []Package{}
	invalid := 0
	for _, id := range ids {
		p, err := Load(root, id)
		if err != nil {
			invalid++
			continue
		}
		if p.Matches(layout, theme) {
			packages = append(packages, p)
		}
	}
	return packages, invalid, nil
}
func ReadResource(root, id, name string) ([]byte, string, error) {
	if !safeResource(name) {
		return nil, "", ErrNotFound
	}
	p, err := Load(root, id)
	if err != nil {
		return nil, "", err
	}
	if !slices.ContainsFunc(p.Resources, func(r Resource) bool { return r.Path == name }) {
		return nil, "", ErrNotFound
	}
	files, close, err := Files(root, id)
	if err != nil {
		return nil, "", err
	}
	defer close()
	raw, err := readFile(files, name)
	return raw, mediaType(name), err
}

//go:embed source.json
var Source json.RawMessage

func License() []byte { raw, _ := builtin.ReadFile("builtin/LICENSE"); return raw }

var sourceCommit = func() string {
	var s struct {
		Commit string `json:"commit"`
	}
	if err := json.Unmarshal(Source, &s); err != nil || len(s.Commit) != 40 {
		panic("invalid skin source metadata")
	}
	return s.Commit
}()
