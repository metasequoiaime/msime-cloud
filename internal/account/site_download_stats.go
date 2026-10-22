package account

import (
	"encoding/json"
	"net/http"
	"time"
)

const siteDownloadStatsPath = "/v1/site/download-stats"

// SiteDownloadStats 是官网累计下载使用的镜像补充计数，不公开事件、版本、账号或访问者资料。
type SiteDownloadStats struct {
	Total       int64            `json:"total"`
	Platforms   map[string]int64 `json:"platforms"`
	GeneratedAt time.Time        `json:"generated_at"`
}

// siteDownloadStats 统计全部历史镜像下载；不设日期窗口，不读取 GitHub 的每日差分。
func (a *Service) siteDownloadStats(w http.ResponseWriter, r *http.Request) {
	rows, err := a.store.pool.Query(r.Context(), `SELECT CASE WHEN lower(platform)='windows-rust' THEN 'windowsRust' ELSE `+canonicalPlatformSQL("platform")+` END AS p,count(*),now()
 FROM admin_events WHERE kind='download' AND channel='cn-mirror' GROUP BY p`)
	if err != nil {
		a.error(w, err)
		return
	}
	defer rows.Close()
	stats := SiteDownloadStats{Platforms: map[string]int64{}, GeneratedAt: time.Now().UTC()}
	for rows.Next() {
		var platform string
		var count int64
		if err := rows.Scan(&platform, &count, &stats.GeneratedAt); err != nil {
			a.error(w, err)
			return
		}
		// 未识别的平台只公开合计，不能把客户端提供的任意文本作为公开字段。
		switch platform {
		case "windows", "windowsRust", "macos", "linux", "android", "ios", "harmony":
		default:
			platform = "other"
		}
		stats.Platforms[platform] += count
		stats.Total += count
	}
	if err := rows.Err(); err != nil {
		a.error(w, err)
		return
	}
	stats.GeneratedAt = stats.GeneratedAt.UTC()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", siteMirrorCacheMaxAge)
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(200)
	json.NewEncoder(w).Encode(stats)
}
