package account

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"net/http"
	"net/url"
	"strconv"

	"golang.org/x/image/draw"
)

// 社区键盘皮肤的来源标记，取值与 community_schema.sql 的 community_skins_origin_check 一致。unknown 是加列之前已有的带照片的作品和滚动升级期间旧副本写入的作品，服务端推不出它们的来源；user 是发布时照片没有匹配到本服务生成过的插画（或没有照片）、也没有声明 AI 生成的作品；ai 是照片匹配到本服务生成过的插画，或客户端声明为 AI 生成的作品。
const (
	skinOriginUnknown = "unknown"
	skinOriginUser    = "user"
	skinOriginAI      = "ai"
)

var skinOrigins = []string{skinOriginUnknown, skinOriginUser, skinOriginAI}

// skinOriginHeader 是支持 `exclude=ai` 和 `fields=origin` 的副本在声明了其中任何一个的响应上返回的能力头。旧副本会忽略不认识的查询参数并照常返回 200，所以客户端不能凭状态码认定过滤已经生效，必须检查这个头。
const (
	skinOriginHeader  = "X-MSIME-Skin-Origin"
	skinOriginVersion = "v1"
)

// skinArtworkMatchDistance 是照片与已记录插画的 dHash 汉明距离上限。客户端只把插画缩放并转成 JPEG，服务端再按 q80 重新编码，这些处理只让少数几位翻转；两张无关图片的距离通常在 20 以上。误判的代价只是鸿蒙少显示一款皮肤、其他平台多一个「AI 生成」标签。
const skinArtworkMatchDistance = 8

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
