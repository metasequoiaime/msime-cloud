package account

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/skins"
	"github.com/pelletier/go-toml/v2"
)

// Limits for user-published candidate-window skin packages. crates/client-services/src/skin/candidate_community.rs mirrors them so the client can refuse a package before uploading it.
const (
	// The manifest can reference only the preview, the decoration image and the background image, and every uploaded file must be referenced, so three files is the real bound.
	maxCandidateFiles         = 3
	maxCandidateFileBytes     = 1 << 20
	maxCandidatePackageBytes  = 2 << 20
	maxCandidatePreviewBytes  = 256 << 10
	maxCandidateManifestBytes = 65536
	maxCandidateSide          = 2048
	maxCandidatePixels        = 8_000_000
	// Covers 2 MiB of images in base64 (2,796,204 bytes) plus a JSON-escaped 64 KiB manifest, keys and metadata.
	maxCandidatePublishBytes = 3_200_000
	// Public rows are the gallery quota; private rows are an account's synced library, so the total bound is higher.
	maxCandidateSkinsPerUser  = 20
	maxCandidateLibraryRows   = 100
	candidatePublishesPerHour = 10
	// Private creates and replacements are background sync traffic, so they get their own hourly budget instead of the gallery's.
	candidateLibraryWritesPerHour = 60
	// 官方发布账号（auth.community.official_skin_publishers）不受公开 20 款的限制，公开作品只受每账号总数上限约束；两份每小时额度都换成这个更宽的值，足够在一小时内把整个库同步、替换并公开一遍，同时让配置失误或客户端死循环仍有上限。
	candidateOfficialWritesPerHour = 200
)

// candidateImageSlots 限制每个进程的图片解码并发：候选窗皮肤、反馈截图与头像共用两个槽，因为这些账号路由绕过服务器的 MaxConcurrent，单张头像解码就可能占用 128 MiB 像素缓冲。
var candidateImageSlots = make(chan struct{}, 2)

// candidatePublishTimeout is how long one publish may take end to end: up to 3.2 MB of upload, then decoding and re-encoding up to 8 MP. The default 15 s route context and the server's read deadline would cut a slow uplink off mid-upload; the client waits 90 s.
const candidatePublishTimeout = 90 * time.Second

func acquireCandidateImageSlot(ctx context.Context) bool {
	select {
	case candidateImageSlots <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// withCandidateImageSlot runs work while holding an image slot and gives the slot back however work ends, a panic included, so a failed decode can never shrink the pool. It reports false, without running work, when ctx ends before a slot frees.
func withCandidateImageSlot(ctx context.Context, work func()) bool {
	if !acquireCandidateImageSlot(ctx) {
		return false
	}
	defer func() { <-candidateImageSlots }()
	work()
	return true
}

type CandidateSkinLicense struct {
	Code   string `json:"code"`
	Assets string `json:"assets"`
	Source string `json:"source"`
}

// CommunityCandidateSkin is the list and detail item; it never carries the manifest or image bytes. Visibility, UpdatedAt and RequestSHA256 are the sync fields: released clients decode items with unknown fields denied, so a response carries them only when the client opted in (see candidateLegacy). RequestSHA256 is set only for the owner.
type CommunityCandidateSkin struct {
	ID            string               `json:"id"`
	PackageID     string               `json:"package_id"`
	Name          string               `json:"name"`
	Description   string               `json:"description"`
	Author        string               `json:"author"`
	Version       string               `json:"version"`
	License       CandidateSkinLicense `json:"license"`
	Size          int64                `json:"size"`
	FileCount     int                  `json:"file_count"`
	Downloads     int                  `json:"downloads"`
	RatingCount   int                  `json:"rating_count"`
	RatingAverage float64              `json:"rating_average"`
	Owned         bool                 `json:"owned"`
	MyRating      int                  `json:"my_rating"`
	CreatedAt     time.Time            `json:"created_at"`
	Visibility    string               `json:"visibility,omitempty"`
	UpdatedAt     time.Time            `json:"updated_at,omitzero"`
	RequestSHA256 string               `json:"request_sha256,omitempty"`
	// Moderation 是审核状态，只出现在作者自己的作品上，且只在带 `fields=moderation` 时出现（见 communityFields）。
	Moderation string `json:"moderation,omitempty"`
	moderation string
	// Category 是图库分类，同样只在客户端用 include=category 声明支持时出现（见 candidateItem）。
	Category string `json:"category,omitempty"`
	// Saved 和 Saves 是当前用户是否收藏（匿名为 false）和收藏总数，只在请求带 `fields=saved` 时出现（见 communitySavedFields）。
	Saved *bool `json:"saved,omitempty"`
	Saves *int  `json:"saves,omitempty"`
	saved bool
	saves int
}

// candidateSkinCategories 是图库分类的全部取值，候选窗皮肤与社区键盘皮肤共用，与 community_candidate_skin_schema.sql 的 community_candidate_skins_category_check、community_schema.sql 的 community_skins_category_check 一致。分类只是发布元数据，不属于 skin.toml 或键盘皮肤的 design；缺省为 other。
var candidateSkinCategories = []string{"nature", "guofeng", "acg", "cute", "food", "tech", "minimal", "other"}

const defaultCandidateSkinCategory = "other"

func validCandidateSkinCategory(category string) bool {
	return slices.Contains(candidateSkinCategories, category)
}

// candidateIncludeCategory 读取 include=category；ok 为 false 表示 include 带了其他非空值。
func candidateIncludeCategory(r *http.Request) (category bool, ok bool) {
	switch r.URL.Query().Get("include") {
	case "":
		return false, true
	case "category":
		return true, true
	}
	return false, false
}

// candidateItem 按客户端的声明裁剪条目：没有 sync 时去掉同步字段，没有 include=category 时去掉分类，未声明的响应因此与加入这些字段之前逐字节相同。
func candidateItem(v CommunityCandidateSkin, sync, category bool) CommunityCandidateSkin {
	if !sync {
		v = candidateLegacy(v)
	}
	if !category {
		v.Category = ""
	}
	return v
}

// candidateLegacy strips the sync fields, so a client that did not opt in gets the item exactly as before private rows existed.
func candidateLegacy(v CommunityCandidateSkin) CommunityCandidateSkin {
	v.Visibility, v.UpdatedAt, v.RequestSHA256 = "", time.Time{}, ""
	return v
}

// candidateFields 读取列表和详情的 `fields=` 开关：sync（同步字段和作者的私有作品）、moderation 与 saved，可单独使用，也可逗号组合，例如 `fields=sync,moderation`；出现其他名称时 ok 为 false。
func candidateFields(r *http.Request) (fields map[string]bool, ok bool) {
	return communityFields(r, "sync", "moderation", "saved")
}

const candidateSkinSelectBase = `SELECT s.id,s.package_id,s.name,s.description,
 COALESCE(NULLIF(btrim(u.display_name),''),'水杉小鹿·'||upper(left(u.id,6))),s.version,s.license_code,s.license_assets,s.license_source,
 f.size,f.files,
 (SELECT count(*) FROM community_candidate_skin_downloads WHERE skin_id=s.id),
 rating_stats.count,rating_stats.average,s.owner_id=$1,rating_stats.mine,s.created_at,
 s.visibility,s.updated_at,CASE WHEN s.owner_id=$1 THEN s.request_sha256 ELSE '' END,s.category,
 CASE WHEN s.owner_id=$1 THEN s.moderation ELSE '' END,`

// candidateSkinSelect 只有客户端声明 fields=saved 时才计算收藏总数和当前用户的收藏状态；默认目录不需要这两个逐行子查询。
func candidateSkinSelect(saved bool) string {
	return candidateSkinSelectQuery(saved, false)
}

func candidateSkinSelectQuery(saved, page bool) string {
	from := ` FROM community_candidate_skins s JOIN auth_users u ON u.id=s.owner_id `
	skinID := "s.id"
	if page {
		from += `JOIN candidate_page p ON p.id=s.id `
		// 文件和评分依赖本页 ID，避免优化器将聚合排到分页关联之前。
		skinID = "p.id"
	}
	from += `CROSS JOIN LATERAL (SELECT COALESCE(sum(size),0) AS size,count(*) AS files FROM community_candidate_skin_files WHERE skin_id=` + skinID + `) f `
	// 人数、平均分和个人评分共用一次聚合；主键保证每个用户每款皮肤只有一条评分。
	from += `CROSS JOIN LATERAL (SELECT count(*) AS count,COALESCE(avg(stars),0) AS average,
 COALESCE(max(stars) FILTER (WHERE user_id=$1),0) AS mine FROM community_candidate_skin_ratings WHERE skin_id=` + skinID + `) rating_stats `
	if saved {
		return candidateSkinSelectBase + `(SELECT count(*) FROM community_candidate_skin_saves WHERE skin_id=s.id),EXISTS(SELECT 1 FROM community_candidate_skin_saves WHERE skin_id=s.id AND user_id=$1)` + from
	}
	return candidateSkinSelectBase + `0,false` + from
}

func scanCandidateSkin(row interface{ Scan(...any) error }) (CommunityCandidateSkin, error) {
	var s CommunityCandidateSkin
	err := row.Scan(&s.ID, &s.PackageID, &s.Name, &s.Description, &s.Author, &s.Version, &s.License.Code, &s.License.Assets, &s.License.Source, &s.Size, &s.FileCount, &s.Downloads, &s.RatingCount, &s.RatingAverage, &s.Owned, &s.MyRating, &s.CreatedAt, &s.Visibility, &s.UpdatedAt, &s.RequestSHA256, &s.Category, &s.moderation, &s.saves, &s.saved)
	return s, err
}

// candidateExtension is the lowercased extension of an accepted image path, or "" when the path is not png, jpg or jpeg.
func candidateExtension(path string) string {
	i := strings.LastIndexByte(path, '.')
	if i < 0 {
		return ""
	}
	switch ext := strings.ToLower(path[i+1:]); ext {
	case "png":
		return "png"
	case "jpg", "jpeg":
		return "jpeg"
	}
	return ""
}

// validCandidateFiles runs the structural checks that need no image decode and returns the error code, or "" when the upload may proceed.
func validCandidateFiles(manifest string, files map[string][]byte) string {
	if len(files) < 1 || len(files) > maxCandidateFiles {
		return "candidate_skin_file_path"
	}
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	lowered := map[string]bool{}
	total := 0
	for _, path := range paths {
		if !skins.SafeResource(path, 256) || path == "skin.toml" {
			return "candidate_skin_file_path"
		}
		if candidateExtension(path) == "" {
			return "candidate_skin_file_type"
		}
		loweredPath := strings.ToLower(path)
		if lowered[loweredPath] {
			return "candidate_skin_file_path"
		}
		lowered[loweredPath] = true
		size := len(files[path])
		total += size
		if size < 1 || size > maxCandidateFileBytes || total > maxCandidatePackageBytes {
			return "candidate_skin_too_large"
		}
	}
	if len(manifest) > maxCandidateManifestBytes {
		return "candidate_skin_too_large"
	}
	return ""
}

// candidateRequestDigest identifies one publish request by its original bytes, so an identical retry is recognised even though the stored images are re-encoded.
func candidateRequestDigest(name, description, manifest string, files map[string][]byte) string {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	h := sha256.New()
	for _, part := range []string{name, description, manifest} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	for _, path := range paths {
		sum := sha256.Sum256(files[path])
		h.Write([]byte(path + "\x00" + hex.EncodeToString(sum[:]) + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// validCandidatePackage validates the manifest with the shared client-dialect port, then applies the v1 sharing rules around it: images only, a required image preview, a declared asset license when requireLicense (every public row), and no unreferenced files.
func validCandidatePackage(manifest string, files map[string][]byte, requireLicense bool) (skins.Package, string) {
	var head struct {
		ID string `toml:"id"`
	}
	if toml.Unmarshal([]byte(manifest), &head) != nil || head.ID == "" {
		return skins.Package{}, "invalid_candidate_skin_package"
	}
	resources := make([]skins.StoredResource, 0, len(files))
	for path, data := range files {
		sum := sha256.Sum256(data)
		resources = append(resources, skins.StoredResource{Path: path, Size: len(data), SHA256: hex.EncodeToString(sum[:])})
	}
	pkg, err := skins.ParseStored(skins.Stored{ID: head.ID, Manifest: []byte(manifest), Resources: resources})
	if err != nil {
		return pkg, "invalid_candidate_skin_package"
	}
	if pkg.ToolbarStylesheet != "" {
		return pkg, "candidate_skin_file_type"
	}
	if pkg.Preview == "" {
		return pkg, "candidate_skin_preview_required"
	}
	if _, uploaded := files[pkg.Preview]; !uploaded || candidateExtension(pkg.Preview) == "" {
		return pkg, "candidate_skin_file_type"
	}
	license := candidatePackageLicense(pkg)
	if requireLicense && strings.TrimSpace(license.Assets) == "" {
		return pkg, "candidate_skin_license_required"
	}
	// ParseStored bounds these strings by length only, while clients reject a listed item whose version or license carries a control character, so one such row would fail every gallery page that contains it.
	for _, text := range []string{pkg.Version, license.Code, license.Assets, license.Source} {
		if strings.ContainsFunc(text, unicode.IsControl) {
			return pkg, "invalid_candidate_skin_package"
		}
	}
	referenced := map[string]bool{pkg.Preview: true}
	if decoration := pkg.CandidateWindow.Decoration.Image; decoration != "" {
		referenced[decoration] = true
	}
	if background := pkg.CandidateWindow.Background; background != nil {
		referenced[background.Image] = true
	}
	if len(referenced) != len(files) {
		return pkg, "invalid_candidate_skin_package"
	}
	for path := range files {
		if !referenced[path] {
			return pkg, "invalid_candidate_skin_package"
		}
	}
	return pkg, ""
}

// candidatePackageLicense is the manifest's [license] table, empty when a private package declares none.
func candidatePackageLicense(pkg skins.Package) CandidateSkinLicense {
	if pkg.License == nil {
		return CandidateSkinLicense{}
	}
	return CandidateSkinLicense{Code: pkg.License.Code, Assets: pkg.License.Assets, Source: pkg.License.Source}
}

// sanitizeCandidateImages 按扩展名选择解码器完整解码每张图片：静态 PNG 和 JPEG 解码后重新编码，从而去掉 EXIF、XMP、ICC 和文本块；APNG 走 parseCandidateAPNG 的块级清理，保留动画并逐帧解码校验。尺寸、APNG 帧数和 JPEG 扫描段数都在完整解码前从文件头读取，APNG 的每幅画面都按整张画布计入整包像素上限。
func sanitizeCandidateImages(files map[string][]byte, preview string) (map[string][]byte, string) {
	pixels := 0
	animated := map[string]*candidateAPNG{}
	for path, data := range files {
		var config image.Config
		var err error
		images := 1
		if candidateExtension(path) == "png" && candidateAnimatedPNG(data) {
			a, code := parseCandidateAPNG(data)
			if code != "" {
				return nil, code
			}
			animated[path] = a
			config.Width, config.Height, images = a.width, a.height, a.images
		} else if candidateExtension(path) == "png" {
			config, err = png.DecodeConfig(bytes.NewReader(data))
		} else if jpegScans(data) > maxJPEGScans {
			return nil, "candidate_skin_image_invalid"
		} else {
			config, err = jpeg.DecodeConfig(bytes.NewReader(data))
		}
		if err != nil {
			return nil, "candidate_skin_image_invalid"
		}
		if config.Width < 1 || config.Height < 1 || config.Width > maxCandidateSide || config.Height > maxCandidateSide {
			return nil, "candidate_skin_too_large"
		}
		pixels += config.Width * config.Height * images
		if pixels > maxCandidatePixels {
			return nil, "candidate_skin_too_large"
		}
	}
	clean := make(map[string][]byte, len(files))
	total := 0
	for path, data := range files {
		var decoded image.Image
		var err error
		var out bytes.Buffer
		if a := animated[path]; a != nil {
			if !a.decode() {
				return nil, "candidate_skin_image_invalid"
			}
			out.Write(a.clean)
		} else if candidateExtension(path) == "png" {
			if decoded, err = png.Decode(bytes.NewReader(data)); err == nil {
				err = (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&out, decoded)
			}
		} else {
			if decoded, err = jpeg.Decode(bytes.NewReader(data)); err == nil {
				err = jpeg.Encode(&out, decoded, &jpeg.Options{Quality: 90})
			}
		}
		if err != nil {
			return nil, "candidate_skin_image_invalid"
		}
		total += out.Len()
		if out.Len() > maxCandidateFileBytes || total > maxCandidatePackageBytes || (path == preview && out.Len() > maxCandidatePreviewBytes) {
			return nil, "candidate_skin_too_large"
		}
		clean[path] = out.Bytes()
	}
	return clean, ""
}

func (a *Service) communityCandidateList(w http.ResponseWriter, r *http.Request) {
	offset, search, code := communityListPage(r)
	if code != "" {
		writeError(w, 400, code)
		return
	}
	scope, validScope := communityListScope(r)
	if !validScope {
		writeError(w, 400, "invalid_scope")
		return
	}
	fields, ok := candidateFields(r)
	if !ok {
		writeError(w, 400, "invalid_fields")
		return
	}
	sync := fields["sync"]
	category, withCategory, code := communitySkinListCategory(r)
	if code != "" {
		writeError(w, 400, code)
		return
	}
	viewer := a.communityViewer(r)
	if scope != "" && viewer == "" {
		writeError(w, 401, "user_session_required")
		return
	}
	// 私有作品只出现在作者自己、且声明了 fields=sync 的 mine 或 saved 列表里：已发布的客户端拒绝 license assets 为空的条目，而私有作品可以为空，一条就会让整页解析失败。
	join, order := communitySavedScope(scope, "community_candidate_skin_saves", "skin_id", "s")
	// 先选本页皮肤，再计算文件与互动统计，避免处理 OFFSET 跳过的包。
	rows, e := a.store.pool.Query(r.Context(), `WITH candidate_page AS MATERIALIZED (
 SELECT s.id FROM community_candidate_skins s JOIN auth_users u ON u.id=s.owner_id `+join+`
 WHERE strpos(lower(s.name),lower($2))>0 AND ($3<>'mine' OR s.owner_id=$1) AND (s.visibility='public' OR ($3<>'' AND $5 AND s.owner_id=$1)) AND (s.moderation<>'removed' OR s.owner_id=$1) AND ($6='' OR s.category=$6)
 ORDER BY `+order+` LIMIT 21 OFFSET $4
) `+candidateSkinSelectQuery(fields["saved"], true)+join+`ORDER BY `+order, viewer, search, scope, offset, sync, category)
	if e != nil {
		a.error(w, e)
		return
	}
	defer rows.Close()
	items := []CommunityCandidateSkin{}
	for rows.Next() {
		v, e := scanCandidateSkin(rows)
		if e != nil {
			a.error(w, e)
			return
		}
		v = candidateItem(v, sync, withCategory)
		v.Moderation = ownerModeration(fields, v.moderation)
		v.Saved, v.Saves = communitySavedFields(fields, v.saved, v.saves)
		items = append(items, v)
	}
	if e = rows.Err(); e != nil {
		a.error(w, e)
		return
	}
	more := len(items) > 20
	if more {
		items = items[:20]
	}
	write(w, 200, map[string]any{"skins": items, "has_more": more})
}
func (a *Service) communityCandidateDetail(w http.ResponseWriter, r *http.Request) {
	fields, ok := candidateFields(r)
	if !ok {
		writeError(w, 400, "invalid_fields")
		return
	}
	sync := fields["sync"]
	withCategory, ok := candidateIncludeCategory(r)
	if !ok {
		writeError(w, 400, "invalid_include")
		return
	}
	// A private row is the owner's alone, and only an opted-in client can decode it; everyone else gets the same 404 as for a missing id.
	v, e := scanCandidateSkin(a.store.pool.QueryRow(r.Context(), candidateSkinSelect(fields["saved"])+`WHERE s.id=$2 AND (s.visibility='public' OR ($3 AND s.owner_id=$1)) AND (s.moderation<>'removed' OR s.owner_id=$1)`, a.communityViewer(r), r.PathValue("id"), sync))
	if errors.Is(e, pgx.ErrNoRows) {
		writeError(w, 404, "skin_not_found")
		return
	}
	if e != nil {
		a.error(w, e)
		return
	}
	v = candidateItem(v, sync, withCategory)
	v.Moderation = ownerModeration(fields, v.moderation)
	v.Saved, v.Saves = communitySavedFields(fields, v.saved, v.saves)
	write(w, 200, v)
}

// communityCandidateSave 收藏或取消收藏候选窗皮肤。别人的私有作品和已下架的作品返回 404，作者本人的不受限制。
func (a *Service) communityCandidateSave(w http.ResponseWriter, r *http.Request) {
	a.communitySave(w, r, communitySaveTarget{items: "community_candidate_skins", saves: "community_candidate_skin_saves", column: "skin_id", visible: "(s.visibility='public' OR s.owner_id=$2) AND (s.moderation<>'removed' OR s.owner_id=$2)", notFound: "skin_not_found"})
}
func (a *Service) communityCandidatePreview(w http.ResponseWriter, r *http.Request) {
	var path string
	var data []byte
	e := a.store.pool.QueryRow(r.Context(), `SELECT f.path,f.bytes FROM community_candidate_skins s JOIN community_candidate_skin_files f ON f.skin_id=s.id AND f.path=s.preview_path WHERE s.id=$1 AND ((s.visibility='public' AND s.moderation<>'removed') OR s.owner_id=$2)`, r.PathValue("id"), a.communityViewer(r)).Scan(&path, &data)
	if errors.Is(e, pgx.ErrNoRows) {
		writeError(w, 404, "skin_not_found")
		return
	}
	if e != nil {
		a.error(w, e)
		return
	}
	write(w, 200, map[string]any{"path": path, "content_type": "image/" + candidateExtension(path), "data": data})
}
func (a *Service) communityCandidatePublish(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	var input struct {
		candidateUpload
		ID string `json:"id"`
		// Absent means public. Sending it is also the client's opt-in to the sync fields in the response.
		Visibility *string `json:"visibility"`
		// 缺省为 other。分类不计入 request_sha256：同一内容的重试无论带什么分类都返回已存的作品，之后改分类走 PATCH。
		Category *string `json:"category"`
	}
	withCategory, ok := readCandidateUpload(w, r, &input)
	if !ok {
		return
	}
	sync, visibility := input.Visibility != nil, "public"
	if sync {
		visibility = *input.Visibility
	}
	if visibility != "public" && visibility != "private" {
		writeError(w, 400, "invalid_visibility")
		return
	}
	category := defaultCandidateSkinCategory
	if input.Category != nil {
		category = *input.Category
	}
	if !validCandidateSkinCategory(category) {
		writeError(w, 400, "invalid_category")
		return
	}
	respond := func(status int, v CommunityCandidateSkin) {
		write(w, status, candidateItem(v, sync, withCategory))
	}
	input.ID = strings.ToLower(input.ID)
	if len(input.ID) != 36 || !validCommunityID(input.ID) {
		writeError(w, 400, "invalid_community_id")
		return
	}
	pkg, digest, code := input.prepare(visibility == "public")
	if code != "" {
		writeError(w, 400, code)
		return
	}
	// 已提交的重试先在同一快照中校验作者、摘要并读取响应，不扣减额度；事务内探测仍为最终依据。
	row, e := a.store.communityPublishedItem(r.Context(), "community_candidate_skins", candidateSkinSelect(false)+`WHERE s.id=(SELECT id FROM publish_match)`, p.UserID, input.ID, digest)
	if e == nil {
		v, e := scanCandidateSkin(row)
		if e != nil {
			a.error(w, e)
			return
		}
		respond(200, v)
		return
	}
	if errors.Is(e, errCommunityPublishConflict) {
		writeError(w, 409, "candidate_skin_id_conflict")
		return
	}
	if !errors.Is(e, pgx.ErrNoRows) {
		a.error(w, e)
		return
	}
	if e := a.candidateWriteRate(r.Context(), p.UserID, visibility); e != nil {
		a.error(w, e)
		return
	}
	clean, flag, ok := a.screenCandidateUpload(w, r, input.candidateUpload, pkg.Preview)
	if !ok {
		return
	}
	tx, e := a.store.userDataTransaction(r.Context(), p.UserID)
	if e != nil {
		a.error(w, e)
		return
	}
	defer tx.Rollback(r.Context())
	license := candidatePackageLicense(pkg)
	publicLimit := a.candidatePublicLimit(p.UserID)
	// 事务内合并 ID 冲突、账号配额检查和首发写入，避免锁定用户后再为两步各往返一次。
	var existingOwner, existingDigest string
	var identical, exists, withinTotal, withinPublic, inserted bool
	if e = tx.QueryRow(r.Context(), `WITH existing AS MATERIALIZED (
 SELECT owner_id,request_sha256,request_sha256=$12 AS identical
 FROM community_candidate_skins WHERE id=$1
), quota AS MATERIALIZED (
 SELECT count(*) AS total_count,count(*) FILTER (WHERE visibility='public') AS public_count
 FROM community_candidate_skins WHERE owner_id=$2
), inserted AS (
 INSERT INTO community_candidate_skins(id,owner_id,package_id,name,description,version,license_code,license_assets,license_source,manifest,preview_path,request_sha256,visibility,moderation,moderation_reason,category)
 SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'pending',$14,$15 FROM quota
 WHERE NOT EXISTS(SELECT 1 FROM existing) AND total_count<$16 AND ($13<>'public' OR public_count<$17)
 ON CONFLICT(id) DO NOTHING
 RETURNING id
)
	SELECT COALESCE((SELECT owner_id FROM existing),''),COALESCE((SELECT request_sha256 FROM existing),''),
 COALESCE((SELECT identical FROM existing),false),EXISTS(SELECT 1 FROM existing),
 (SELECT total_count<$16 FROM quota),(SELECT $13<>'public' OR public_count<$17 FROM quota),EXISTS(SELECT 1 FROM inserted)`,
		input.ID, p.UserID, pkg.ID, input.Name, input.Description, pkg.Version, license.Code, license.Assets, license.Source, []byte(input.Manifest), pkg.Preview, digest, visibility, flag, category, maxCandidateLibraryRows, publicLimit).Scan(&existingOwner, &existingDigest, &identical, &exists, &withinTotal, &withinPublic, &inserted); e != nil {
		a.error(w, e)
		return
	}
	if exists {
		if existingOwner != p.UserID || !identical {
			writeError(w, 409, "candidate_skin_id_conflict")
			return
		}
		v, e := scanCandidateSkin(tx.QueryRow(r.Context(), candidateSkinSelect(false)+`WHERE s.id=$2`, p.UserID, input.ID))
		if e != nil {
			a.error(w, e)
			return
		}
		respond(200, v)
		return
	}
	if !withinTotal {
		writeError(w, 409, "candidate_skin_library_limit")
		return
	}
	if !withinPublic {
		writeError(w, 409, "candidate_skin_publish_limit")
		return
	}
	// The row lock covers only this account, so another account can commit the same id between the probe and this insert; ON CONFLICT waits for that commit and then reports it as a conflict instead of a unique violation.
	if !inserted {
		writeError(w, 409, "candidate_skin_id_conflict")
		return
	}
	if e = insertCandidateFiles(r.Context(), tx, input.ID, clean); e != nil {
		a.error(w, e)
		return
	}
	v, e := commitCandidateSkin(r.Context(), tx, p.UserID, input.ID)
	if e != nil {
		a.error(w, e)
		return
	}
	respond(201, v)
}

// candidateWriteRate 把一次包写入计入对应的每小时额度：变为公开的计入图库额度，私有创建、替换和改分类计入私有库额度。官方发布账号的两份额度都是 candidateOfficialWritesPerHour。
func (a *Service) candidateWriteRate(ctx context.Context, userID, visibility string) error {
	scope, limit := a.candidateWriteRatePolicy(userID, visibility)
	return a.RateLimit(ctx, scope, userID, limit, time.Hour)
}

func (a *Service) candidateWriteRateTx(ctx context.Context, tx pgx.Tx, userID, visibility string) error {
	scope, limit := a.candidateWriteRatePolicy(userID, visibility)
	return rateTx(ctx, tx, scope+":"+hash(userID), limit, time.Hour)
}

// candidateWriteRatePolicy 统一普通与事务写入的额度选择；计数仍由各自的数据库调用执行。
func (a *Service) candidateWriteRatePolicy(userID, visibility string) (string, int) {
	scope, limit := "candidate-library", candidateLibraryWritesPerHour
	if visibility == "public" {
		scope, limit = "candidate-publish", candidatePublishesPerHour
	}
	if a.config.Community.officialSkinPublisher(userID) {
		limit = candidateOfficialWritesPerHour
	}
	return scope, limit
}

// candidatePublicLimit 返回账号最多可以有几款公开作品：普通账号 maxCandidateSkinsPerUser，官方发布账号只受总数上限 maxCandidateLibraryRows 约束。
func (a *Service) candidatePublicLimit(userID string) int {
	if a.config.Community.officialSkinPublisher(userID) {
		return maxCandidateLibraryRows
	}
	return maxCandidateSkinsPerUser
}

// insertCandidateFiles stores the re-encoded images of one package in path order.
func insertCandidateFiles(ctx context.Context, tx pgx.Tx, id string, clean map[string][]byte) error {
	query, args := candidateFilesInsertQuery(id, clean)
	_, err := tx.Exec(ctx, query, args...)
	return err
}

func candidateFilesInsertQuery(id string, clean map[string][]byte) (string, []any) {
	paths := make([]string, 0, len(clean))
	for path := range clean {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	contents := make([][]byte, len(paths))
	for i, path := range paths {
		contents[i] = clean[path]
	}
	return `INSERT INTO community_candidate_skin_files(skin_id,path,bytes)
SELECT $1,files.path,files.bytes
FROM unnest($2::text[],$3::bytea[]) AS files(path,bytes)`, []any{id, paths, contents}
}
func (a *Service) communityCandidateDownload(w http.ResponseWriter, r *http.Request) {
	p, authErr := a.Authenticate(r.Context(), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	// 无有效凭据时允许匿名下载；查询失败或取消不能伪装成未登录。
	if authErr != nil && !errors.Is(authErr, ErrInvalid) && !errors.Is(authErr, ErrBanned) {
		a.error(w, authErr)
		return
	}
	viewer := ""
	if authErr == nil {
		viewer = p.UserID
	}
	var tx pgx.Tx
	var e error
	if viewer == "" {
		tx, e = a.store.pool.Begin(r.Context())
	} else {
		tx, e = a.store.userDataTransaction(r.Context(), viewer)
	}
	if e != nil {
		a.error(w, e)
		return
	}
	defer tx.Rollback(r.Context())
	var packageID string
	var manifest []byte
	files := map[string][]byte{}
	batch := &pgx.Batch{}
	batch.Queue(`SELECT package_id,manifest FROM community_candidate_skins WHERE id=$1 AND ((visibility='public' AND moderation<>'removed') OR owner_id=$2) FOR SHARE`, r.PathValue("id"), viewer).QueryRow(func(row pgx.Row) error {
		return row.Scan(&packageID, &manifest)
	})
	batch.Queue(`SELECT path,bytes FROM community_candidate_skin_files WHERE skin_id=$1 ORDER BY path`, r.PathValue("id")).Query(func(rows pgx.Rows) error {
		defer rows.Close()
		for rows.Next() {
			var path string
			var data []byte
			if e := rows.Scan(&path, &data); e != nil {
				return e
			}
			files[path] = data
		}
		return rows.Err()
	})
	// 下载人数仍按有效账号去重；匿名请求不创建账号或下载记录。
	if viewer != "" {
		batch.Queue(`INSERT INTO community_candidate_skin_downloads(skin_id,user_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, r.PathValue("id"), viewer)
	}
	e = tx.SendBatch(r.Context(), batch).Close()
	if errors.Is(e, pgx.ErrNoRows) {
		writeError(w, 404, "skin_not_found")
		return
	}
	if e == nil {
		e = tx.Commit(r.Context())
	}
	if e != nil {
		a.error(w, e)
		return
	}
	write(w, 200, map[string]any{"id": r.PathValue("id"), "package_id": packageID, "manifest": string(manifest), "files": files})
}
func (a *Service) communityCandidateRate(w http.ResponseWriter, r *http.Request) {
	a.rateCommunityItem(w, r, `WITH eligible AS MATERIALIZED (
 SELECT id FROM community_candidate_skins WHERE id=$1 AND visibility='public' AND moderation<>'removed' AND owner_id<>$2
), written AS (
 INSERT INTO community_candidate_skin_ratings(skin_id,user_id,stars)
 SELECT id,$2,$3 FROM eligible
 ON CONFLICT(skin_id,user_id) DO UPDATE SET stars=excluded.stars
 RETURNING skin_id
)
SELECT EXISTS(SELECT 1 FROM written),EXISTS(SELECT 1 FROM community_candidate_skins WHERE id=$1 AND visibility='public' AND moderation<>'removed')`, "skin_not_found", "download_before_rating_or_own_skin")
}
func (a *Service) communityCandidateDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	rows, e := a.deleteOwnedCommunity(r.Context(), "community_candidate_skins", r.PathValue("id"), p.UserID)
	if e != nil {
		a.error(w, e)
		return
	}
	if rows == 0 {
		writeError(w, 404, "skin_not_found")
		return
	}
	write(w, 200, map[string]bool{"deleted": true})
}

// communityCandidateSync lists every row the account owns, private ones included, as the identifiers a client needs to reconcile its local packages. It is not paginated: an account holds at most maxCandidateLibraryRows rows.
func (a *Service) communityCandidateSync(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	rows, e := a.store.pool.Query(r.Context(), `SELECT id,package_id,request_sha256,visibility,updated_at FROM community_candidate_skins WHERE owner_id=$1 ORDER BY updated_at DESC,id`, p.UserID)
	if e != nil {
		a.error(w, e)
		return
	}
	defer rows.Close()
	type entry struct {
		ID            string    `json:"id"`
		PackageID     string    `json:"package_id"`
		RequestSHA256 string    `json:"request_sha256"`
		Visibility    string    `json:"visibility"`
		UpdatedAt     time.Time `json:"updated_at"`
	}
	items := []entry{}
	for rows.Next() {
		var v entry
		if e = rows.Scan(&v.ID, &v.PackageID, &v.RequestSHA256, &v.Visibility, &v.UpdatedAt); e != nil {
			a.error(w, e)
			return
		}
		items = append(items, v)
	}
	if e = rows.Err(); e != nil {
		a.error(w, e)
		return
	}
	write(w, 200, map[string]any{"skins": items})
}

// communityCandidateReplace overwrites the package of a row the caller owns with a new upload of the same package id, validated exactly like a publish. The id, visibility, creation time, downloads and ratings stay; an upload identical to the stored one is answered without a write.
func (a *Service) communityCandidateReplace(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	var input candidateUpload
	withCategory, ok := readCandidateUpload(w, r, &input)
	if !ok {
		return
	}
	id := r.PathValue("id")
	pkg, digest, code := input.prepare(false)
	if code != "" {
		writeError(w, 400, code)
		return
	}
	// check answers from the stored row: done reports that the response is already written, either an error or the unchanged item.
	check := func(q interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	}, lock string) (done bool) {
		var packageID, visibility, stored string
		e := q.QueryRow(r.Context(), `SELECT package_id,visibility,request_sha256 FROM community_candidate_skins WHERE id=$1 AND owner_id=$2`+lock, id, p.UserID).Scan(&packageID, &visibility, &stored)
		switch {
		case errors.Is(e, pgx.ErrNoRows):
			writeError(w, 404, "skin_not_found")
		case e != nil:
			a.error(w, e)
		case packageID != pkg.ID:
			writeError(w, 409, "candidate_skin_package_mismatch")
		case stored == digest:
			v, e := scanCandidateSkin(q.QueryRow(r.Context(), candidateSkinSelect(false)+`WHERE s.id=$2`, p.UserID, id))
			if e != nil {
				a.error(w, e)
			} else {
				write(w, 200, candidateItem(v, true, withCategory))
			}
		case visibility == "public" && strings.TrimSpace(candidatePackageLicense(pkg).Assets) == "":
			writeError(w, 400, "candidate_skin_license_required")
		default:
			return false
		}
		return true
	}
	if check(a.store.pool, "") {
		return
	}
	if e := a.candidateWriteRate(r.Context(), p.UserID, "private"); e != nil {
		a.error(w, e)
		return
	}
	clean, flag, ok := a.screenCandidateUpload(w, r, input, pkg.Preview)
	if !ok {
		return
	}
	tx, e := a.store.userDataTransaction(r.Context(), p.UserID)
	if e != nil {
		a.error(w, e)
		return
	}
	defer tx.Rollback(r.Context())
	// The account lock serialises this with the author's other writes; the row lock also holds off a download reading half-replaced files.
	if check(tx, " FOR UPDATE") {
		return
	}
	license := candidatePackageLicense(pkg)
	batch := &pgx.Batch{}
	batch.Queue(`UPDATE community_candidate_skins SET name=$2,description=$3,version=$4,license_code=$5,license_assets=$6,license_source=$7,manifest=$8,preview_path=$9,request_sha256=$10,updated_at=clock_timestamp(),`+reviewAgain("$11")+` WHERE id=$1`,
		id, input.Name, input.Description, pkg.Version, license.Code, license.Assets, license.Source, []byte(input.Manifest), pkg.Preview, digest, flag)
	batch.Queue(`DELETE FROM community_candidate_skin_files WHERE skin_id=$1`, id)
	query, args := candidateFilesInsertQuery(id, clean)
	batch.Queue(query, args...)
	if e = tx.SendBatch(r.Context(), batch).Close(); e != nil {
		a.error(w, e)
		return
	}
	v, e := commitCandidateSkin(r.Context(), tx, p.UserID, id)
	if e != nil {
		a.error(w, e)
		return
	}
	write(w, 200, candidateItem(v, true, withCategory))
}

// communityCandidateVisibility 是作者修改自己作品发布元数据的接口，请求体 {"visibility"?, "category"?} 至少带一个键。visibility 在私有库与公开图库之间切换：转为公开需要已声明的 asset license 和空余的公开名额，并计入图库的每小时额度；设为当前值不做修改。category 修改图库分类，实际改变时计入私有库的每小时额度；分类不属于包内容，所以不更新 updated_at、不改 request_sha256，也不重新进入审核。
func (a *Service) communityCandidateVisibility(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	var input struct {
		Visibility *string `json:"visibility"`
		Category   *string `json:"category"`
	}
	withCategory, ok := candidateIncludeCategory(r)
	if !ok {
		writeError(w, 400, "invalid_include")
		return
	}
	if !read(w, r, &input) {
		return
	}
	// 两个键都缺省时与只认 visibility 的旧版本一样返回 invalid_visibility。
	if input.Visibility == nil && input.Category == nil || input.Visibility != nil && *input.Visibility != "public" && *input.Visibility != "private" {
		writeError(w, 400, "invalid_visibility")
		return
	}
	if input.Category != nil && !validCandidateSkinCategory(*input.Category) {
		writeError(w, 400, "invalid_category")
		return
	}
	id := r.PathValue("id")
	tx, e := a.store.userDataTransaction(r.Context(), p.UserID)
	if e != nil {
		a.error(w, e)
		return
	}
	defer tx.Rollback(r.Context())
	var visibility, assets, category string
	var public int
	e = tx.QueryRow(r.Context(), `SELECT s.visibility,s.license_assets,s.category,
	CASE WHEN $3 AND s.visibility<>'public' AND btrim(s.license_assets)<>''
		THEN (SELECT count(*) FROM community_candidate_skins WHERE owner_id=$2 AND visibility='public')
		ELSE 0 END
FROM community_candidate_skins s WHERE s.id=$1 AND s.owner_id=$2 FOR UPDATE`, id, p.UserID, input.Visibility != nil && *input.Visibility == "public").Scan(&visibility, &assets, &category, &public)
	if errors.Is(e, pgx.ErrNoRows) {
		writeError(w, 404, "skin_not_found")
		return
	}
	if e != nil {
		a.error(w, e)
		return
	}
	visibilityChanged := input.Visibility != nil && visibility != *input.Visibility
	categoryChanged := input.Category != nil && category != *input.Category
	if visibilityChanged {
		if *input.Visibility == "public" {
			if strings.TrimSpace(assets) == "" {
				writeError(w, 400, "candidate_skin_license_required")
				return
			}
			if public >= a.candidatePublicLimit(p.UserID) {
				writeError(w, 409, "candidate_skin_publish_limit")
				return
			}
			if e = a.candidateWriteRateTx(r.Context(), tx, p.UserID, "public"); e != nil {
				a.error(w, e)
				return
			}
		}
	}
	if categoryChanged {
		if e = a.candidateWriteRateTx(r.Context(), tx, p.UserID, "private"); e != nil {
			a.error(w, e)
			return
		}
	}
	if visibilityChanged || categoryChanged {
		var nextVisibility, nextCategory any
		if visibilityChanged {
			nextVisibility = *input.Visibility
		}
		if categoryChanged {
			nextCategory = *input.Category
		}
		if _, e = tx.Exec(r.Context(), `UPDATE community_candidate_skins SET visibility=COALESCE($2::text,visibility),category=COALESCE($3::text,category),updated_at=CASE WHEN $2::text IS NOT NULL AND visibility<>$2::text THEN clock_timestamp() ELSE updated_at END,moderation=CASE WHEN $2::text='public' AND visibility<>'public' AND moderation='approved' THEN 'pending' ELSE moderation END WHERE id=$1`, id, nextVisibility, nextCategory); e != nil {
			a.error(w, e)
			return
		}
	}
	v, e := commitCandidateSkin(r.Context(), tx, p.UserID, id)
	if e != nil {
		a.error(w, e)
		return
	}
	write(w, 200, candidateItem(v, true, withCategory))
}
