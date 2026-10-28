package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/image/draw"
)

// 社区键盘皮肤的来源标记，取值与 community_schema.sql 的 community_skins_origin_check 一致。unknown 是加列之前已有的带照片的作品、滚动升级期间旧副本写入的带照片的作品，以及后台回填（backfillSkinOrigins）还没处理到的作品，服务端推不出前两种的来源；user 是发布时照片没有匹配到本服务生成过的插画（或没有照片）、也没有声明 AI 生成的作品；ai 是照片匹配到本服务生成过的插画，或客户端声明为 AI 生成的作品。
const (
	skinOriginUnknown = "unknown"
	skinOriginUser    = "user"
	skinOriginAI      = "ai"
)

var skinOrigins = []string{skinOriginUnknown, skinOriginUser, skinOriginAI}

// skinOriginHeader 是支持 `exclude=ai` 和 `fields=origin` 的副本在声明了其中任何一个的响应上返回的能力头。旧副本不认识这两个参数时的表现不同：`exclude` 被忽略、照常返回 200，`fields=origin` 则因为 `fields` 只认 moderation、saved 而返回 400 `invalid_fields`。所以客户端不能凭 200 认定过滤已经生效，必须检查这个头；同时带了 `fields=origin` 的请求在旧副本上拿到的是 400。
const (
	skinOriginHeader  = "X-MSIME-Skin-Origin"
	skinOriginVersion = "v1"
)

// skinArtworkMatchDistance 是照片与已记录插画的 dHash 汉明距离上限。客户端把插画等比缩到 1024 以内、按 q80 起逐档压成 JPEG，服务端再按 q80 重新编码。拿 172 张真实的角色插画模拟这条处理，q80 下的距离最大是 6，q40、q20 下各有一张到 7 和 8；同一个角色系列里互不相同的插画两两比较，距离不超过 8 的占 4%–9%，不超过 6 的占 1%–3%。上限取 6：漏判（AI 插画在鸿蒙上不带标识地出现）是合规风险，只在客户端压到 q40 以下时才可能发生；误判（用户自己的照片被当成 AI 插画，在鸿蒙上不显示、在其他平台带上标签）由审核员用 `set_skin_origin` 改判。翻转、裁剪、加边这类处理会让距离超过上限，服务端判定只是兜底，见 docs/skin-community.md。
const skinArtworkMatchDistance = 6

// communitySkinFields 读取键盘皮肤列表和详情的 `fields=`：在目录共用的 moderation、saved 之外多认 origin。插件仍用 communityCatalogFields，不接受 origin。
func communitySkinFields(query url.Values) (map[string]bool, bool) {
	return communityFields(query, "moderation", "saved", "origin")
}

// communitySkinExclude 读取 `exclude=`：空值表示不过滤，ai 表示只留下来源为 user 的作品，其他值 ok 为 false。
func communitySkinExclude(query url.Values) (excludeAI bool, ok bool) {
	switch query.Get("exclude") {
	case "":
		return false, true
	case skinOriginAI:
		return true, true
	}
	return false, false
}

// communitySkinOriginOptions 读取键盘皮肤读取接口的 `exclude=`，并在客户端声明了 `fields=origin`（fields 由调用处先校验）或 exclude 时写出能力头；没有声明的响应头与加入来源标记之前相同。
func communitySkinOriginOptions(w http.ResponseWriter, query url.Values, fields map[string]bool) (excludeAI bool, ok bool) {
	excludeAI, ok = communitySkinExclude(query)
	if !ok {
		writeError(w, 400, "invalid_exclude")
		return false, false
	}
	if excludeAI || fields["origin"] {
		w.Header().Set(skinOriginHeader, skinOriginVersion)
		w.Header().Add("Access-Control-Expose-Headers", skinOriginHeader)
	}
	return excludeAI, true
}

// communitySkinOriginFilter 返回接在 WHERE 条件之后的固定片段。排除时只留下 user：ai 是 AI 生成的作品；unknown 是带照片的存量作品和旧副本写入的作品，可能是 AI 插画，宁可不显示。只比较来源列，不读 design，避免为了判断有没有照片而把每一行的大照片从 TOAST 里取出来。`alias` 只接收调用处的固定表别名。
func communitySkinOriginFilter(excludeAI bool, alias string) string {
	if !excludeAI {
		return ""
	}
	return ` AND ` + alias + `.origin='user'`
}

// skinArtworkMatchSQL 判断参数 `hash`（bigint，可为 NULL）是否与 skin_artwork_fingerprints 中的某条插画足够接近。汉明距离用 bit(64) 的文本数 1，兼容不提供 bit_count 的 PostgreSQL 12；表只在发布时整表比较一次，不需要索引。`hash` 只接收调用处的固定占位符。
func skinArtworkMatchSQL(hash string) string {
	return `(` + hash + `::bigint IS NOT NULL AND EXISTS(SELECT 1 FROM skin_artwork_fingerprints f WHERE length(replace((f.hash # ` + hash + `::bigint)::bit(64)::text,'0',''))<=` + strconv.Itoa(skinArtworkMatchDistance) + `))`
}

// skinPublishOriginSQL 是发布写入的 origin 表达式：客户端声明 AI 生成（`declared` 为 true）或照片匹配到插画时为 ai，否则为 user。客户端只能把来源往上抬，不能把匹配到的插画改成 user。
func skinPublishOriginSQL(declared, hash string) string {
	return `CASE WHEN ` + declared + `::boolean OR ` + skinArtworkMatchSQL(hash) + ` THEN 'ai' ELSE 'user' END`
}

// skinArtworkHash 计算 64 位 dHash：把图片缩成 9×8 的灰度图，逐行比较相邻两格的亮度。缩放用 golang.org/x/image/draw 的双线性核，它在缩小时按比例放宽采样范围，相当于按区域取平均，对 JPEG 重新压缩和等比缩放都不敏感。
func skinArtworkHash(source image.Image) int64 {
	small := image.NewGray(image.Rect(0, 0, 9, 8))
	draw.BiLinear.Scale(small, small.Bounds(), source, source.Bounds(), draw.Src, nil)
	var hash uint64
	for y := range 8 {
		for x := range 8 {
			hash <<= 1
			if small.GrayAt(x, y).Y < small.GrayAt(x+1, y).Y {
				hash |= 1
			}
		}
	}
	return int64(hash)
}

// communityDesignPhotoHash 返回已经过 parseCommunityDesign 校验和重新编码的照片的 dHash，没有照片时为 nil。照片尺寸和扫描段数已经受限，这里再解码一次的开销只落在发布这种低频写入上，解析函数本身保持不变。
func communityDesignPhotoHash(design CommunityDesign) (*int64, error) {
	if len(design.Photo) == 0 {
		return nil, nil
	}
	photo, err := jpeg.Decode(bytes.NewReader(design.Photo))
	if err != nil {
		return nil, err
	}
	hash := skinArtworkHash(photo)
	return &hash, nil
}

// RecordSkinArtwork 记下本服务生成的一张皮肤插画的指纹，之后发布的皮肤照片与它相近时标记为 AI 生成。指纹不关联账号：用户可能用匿名账号生成、换正式账号发布；它也不能还原出图片。指纹长期保留，因为生成的皮肤可能保存几周后才发布。
func (a *Service) RecordSkinArtwork(ctx context.Context, artwork image.Image) error {
	_, err := a.store.pool.Exec(ctx, `INSERT INTO skin_artwork_fingerprints(hash) VALUES($1) ON CONFLICT(hash) DO NOTHING`, skinArtworkHash(artwork))
	return err
}

// 后台回填社区键盘皮肤来源的参数。回填按 (created_at,id) 的游标分批推进，游标存在 site_settings 的 skinOriginBackfillKey 一行里，所以每次运行只处理上次之后新出现的 unknown 作品，带照片的存量作品只读一次。只处理创建超过 skinOriginBackfillSettle 的作品：还没提交的插入事务的 created_at 可能早于已经推进过去的游标，留出这段时间让它们先提交，下一轮再处理。多个副本同时运行时由事务级 advisory lock 让给一个。
const (
	skinOriginBackfillKey          = "community_skin_origin_backfill"
	skinOriginBackfillBatch        = 100
	skinOriginBackfillLock   int64 = 0x6d73696d656f7267
	skinOriginBackfillSettle       = 5 * time.Minute
)

// skinOriginBackfillCursor 是回填游标，序列化后存在 site_settings.value 里。
type skinOriginBackfillCursor struct {
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
}

// backfillSkinOrigins 把没有照片、来源还是 unknown 的作品改成 user，与发布时的判定一致；带照片的推不出来源（插画任务早已清理，照片也重新编码过），保持 unknown，由审核员改判。服务启动后和之后每小时在后台运行一次（maintain），覆盖加列之前的存量作品和滚动升级期间旧副本写入的作品。每批是一个独立的短事务，只锁这一批的行；ctx 到期时已提交的批次和游标都保留，下次接着处理。
func (s *Store) backfillSkinOrigins(ctx context.Context) error {
	for {
		more, err := s.backfillSkinOriginBatch(ctx)
		if err != nil || !more {
			return err
		}
	}
}

// backfillSkinOriginBatch 处理游标之后的一批 unknown 作品并推进游标，返回后面是否可能还有。另一个副本正在回填时什么都不做，返回 false。
func (s *Store) backfillSkinOriginBatch(ctx context.Context) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var locked bool
	if err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, skinOriginBackfillLock).Scan(&locked); err != nil || !locked {
		return false, err
	}
	var cursor skinOriginBackfillCursor
	var stored string
	err = tx.QueryRow(ctx, `SELECT value FROM site_settings WHERE key=$1`, skinOriginBackfillKey).Scan(&stored)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return false, err
	default:
		if err = json.Unmarshal([]byte(stored), &cursor); err != nil {
			return false, err
		}
	}
	// 先只按来源列和游标挑出这一批，不读 design；再只对这一批判断有没有照片。
	rows, err := tx.Query(ctx, `SELECT id,created_at FROM community_skins WHERE origin='unknown' AND (created_at,id)>($1,$2) AND created_at<now()-make_interval(secs=>$3) ORDER BY created_at,id LIMIT $4`, cursor.CreatedAt, cursor.ID, skinOriginBackfillSettle.Seconds(), skinOriginBackfillBatch)
	if err != nil {
		return false, err
	}
	var ids []string
	if _, err = pgx.ForEachRow(rows, []any{&cursor.ID, &cursor.CreatedAt}, func() error {
		ids = append(ids, cursor.ID)
		return nil
	}); err != nil || len(ids) == 0 {
		return false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE community_skins SET origin='user' WHERE id=ANY($1) AND origin='unknown' AND NOT design ? 'photo'`, ids); err != nil {
		return false, err
	}
	value, err := json.Marshal(cursor)
	if err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO site_settings(key,value,updated_by) VALUES($1,$2,'system') ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=now(),updated_by=excluded.updated_by`, skinOriginBackfillKey, string(value)); err != nil {
		return false, err
	}
	return len(ids) == skinOriginBackfillBatch, tx.Commit(ctx)
}
