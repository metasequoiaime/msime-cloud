package account

import (
	"context"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Downloads page (unit U6): the download event list and the grouped summary. The telemetry endpoint itself is in admin_telemetry.go.

// downloadsList serves GET /api/downloads.
var downloadsList = adminList{
	query: `SELECT id,platform,version,artifact,channel,created_at FROM admin_events WHERE kind='download'`,
	filters: []listFilter{
		{param: "platform", field: "platform", max: 32},
		{param: "version", field: "version", max: 64},
		{param: "artifact", field: "artifact", max: 64},
		{param: "channel", field: "channel", max: 32},
	},
}

const (
	// downloadsMirrorChannel is the telemetry channel key of the domestic website mirror, the numerator of the mirror share.
	downloadsMirrorChannel = "cn-mirror"
	// downloadsSourceTelemetry rows count download events reported through POST /v1/telemetry/events.
	downloadsSourceTelemetry = "telemetry"
	// downloadsSourceGitHub rows count GitHub Release asset downloads, as differences between adjacent daily snapshots.
	downloadsSourceGitHub = "github_release"
	// downloadsRowLimit caps each source's groups; telemetry dimensions are client text, so the number of groups is not otherwise bounded.
	downloadsRowLimit = 500
)

// downloadsRow is one group of the downloads summary: today's and the last seven days' downloads of one platform, version, artifact and channel.
type downloadsRow struct {
	Source   string  `json:"source"`
	Platform string  `json:"platform"`
	Version  string  `json:"version"`
	Artifact *string `json:"artifact"`
	Channel  *string `json:"channel"`
	Repo     string  `json:"repo,omitempty"`
	Tag      string  `json:"tag,omitempty"`
	Today    int64   `json:"today"`
	Week     int64   `json:"week"`
}

// downloadsTotals sums every download of the week, including groups beyond the row cap; the GitHub fields are the GitHub Release part of the sums and MirrorWeek is the domestic mirror's part of Week.
type downloadsTotals struct {
	Today       int64 `json:"today"`
	Week        int64 `json:"week"`
	GitHubToday int64 `json:"github_today"`
	GitHubWeek  int64 `json:"github_week"`
	MirrorWeek  int64 `json:"mirror_week"`
}

// downloadsSummary is the body of GET /api/downloads/summary. Days are UTC calendar days, like the release snapshots; the week is the seven days ending with Day. MirrorShare is null while no download event in the week names a channel, because a zero share would then be a guess.
type downloadsSummary struct {
	Day             string          `json:"day"`
	Rows            []downloadsRow  `json:"rows"`
	Totals          downloadsTotals `json:"totals"`
	MirrorShare     *float64        `json:"mirror_share"`
	ChannelReported bool            `json:"channel_reported"`
	SnapshotDay     *string         `json:"snapshot_day"`
	Truncated       bool            `json:"truncated"`
}

// adminDownloadsSummary serves GET /api/downloads/summary.
func (a *Service) adminDownloadsSummary(w http.ResponseWriter, r *http.Request, _ string) {
	summary, err := a.downloadsSummary(r.Context(), time.Now())
	if err != nil {
		a.error(w, err)
		return
	}
	write(w, 200, summary)
}

// downloadsSummary groups the download telemetry of the UTC week ending on now's day and adds the GitHub Release channel from the asset snapshots.
func (a *Service) downloadsSummary(ctx context.Context, now time.Time) (downloadsSummary, error) {
	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	weekStart := today.AddDate(0, 0, -6)
	summary := downloadsSummary{Day: today.Format(time.DateOnly), Rows: []downloadsRow{}}
	// One snapshot for every query, so the rows, the totals and the snapshot day agree with each other.
	tx, err := a.store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return summary, err
	}
	defer tx.Rollback(ctx)

	// 平台别名（win、darwin、harmonyos 等）归到规范 ID 下，所以同一平台在每个版本、安装包和渠道下只占一行。
	var telemetryToday, telemetryWeek, telemetryMirrorWeek int64
	var telemetryChannelReported bool
	var github []downloadsRow
	batch := &pgx.Batch{}
	batch.Queue(`WITH grouped AS (
 SELECT `+canonicalPlatformSQL("platform")+` AS p,version,artifact,channel,
   count(*) FILTER (WHERE created_at>=$1) AS today,
   count(*) AS week,
   count(*) FILTER (WHERE channel=$4) AS mirror,
   bool_or(channel IS NOT NULL) AS channel_reported
 FROM admin_events WHERE kind='download' AND created_at>=$2 AND created_at<$3
 GROUP BY p,version,artifact,channel
)
SELECT p,version,artifact,channel,today,week,
 sum(today) OVER ()::bigint,sum(week) OVER ()::bigint,sum(mirror) OVER ()::bigint,
 bool_or(channel_reported) OVER ()
 FROM grouped
	 ORDER BY week DESC,p,version,artifact NULLS LAST,channel NULLS LAST LIMIT $5`, today, weekStart, today.AddDate(0, 0, 1), downloadsMirrorChannel, downloadsRowLimit+1).Query(func(rows pgx.Rows) error {
		defer rows.Close()
		for rows.Next() {
			row := downloadsRow{Source: downloadsSourceTelemetry}
			if err := rows.Scan(&row.Platform, &row.Version, &row.Artifact, &row.Channel, &row.Today, &row.Week, &telemetryToday, &telemetryWeek, &telemetryMirrorWeek, &telemetryChannelReported); err != nil {
				return err
			}
			summary.Rows = append(summary.Rows, row)
		}
		return rows.Err()
	})
	batch.Queue(`SELECT to_char(max(day),'YYYY-MM-DD') FROM release_asset_snapshots`).QueryRow(func(row pgx.Row) error {
		return row.Scan(&summary.SnapshotDay)
	})
	batch.Queue(githubReleaseDownloadsQuery, weekStart, today).Query(func(rows pgx.Rows) error {
		var err error
		github, err = collectGitHubReleaseDownloads(rows, &summary)
		return err
	})
	if err = tx.SendBatch(ctx, batch).Close(); err != nil {
		return summary, err
	}
	if len(summary.Rows) > downloadsRowLimit {
		summary.Rows, summary.Truncated = summary.Rows[:downloadsRowLimit], true
	}
	// 窗口汇总在截断前计算，确保返回行上限不会漏掉总数和渠道标记。
	summary.Totals.MirrorWeek = telemetryMirrorWeek
	summary.ChannelReported = telemetryChannelReported

	summary.Rows = append(summary.Rows, github...)
	summary.Totals.Today = telemetryToday + summary.Totals.GitHubToday
	summary.Totals.Week = telemetryWeek + summary.Totals.GitHubWeek
	if summary.ChannelReported && summary.Totals.Week > 0 {
		share := float64(summary.Totals.MirrorWeek) / float64(summary.Totals.Week)
		summary.MirrorShare = &share
	}
	sort.SliceStable(summary.Rows, func(i, j int) bool {
		if summary.Rows[i].Week != summary.Rows[j].Week {
			return summary.Rows[i].Week > summary.Rows[j].Week
		}
		return summary.Rows[i].Today > summary.Rows[j].Today
	})
	return summary, nil
}

// githubReleaseDownloadsQuery 只为本周出现的资产补一条最近的历史基线，本周快照供基线选择和差分共用，避免遍历全部历史。
const githubReleaseDownloadsQuery = `WITH recent AS MATERIALIZED (
 SELECT repo,tag,asset,day,download_count FROM release_asset_snapshots WHERE day>=$1 AND day<=$2
), base AS (
 SELECT a.repo,a.tag,a.asset,p.day,p.download_count
 FROM (SELECT DISTINCT repo,tag,asset FROM recent) a
 CROSS JOIN LATERAL (
   SELECT day,download_count FROM release_asset_snapshots s
   WHERE s.repo=a.repo AND s.tag=a.tag AND s.asset=a.asset AND s.day<$1 ORDER BY day DESC LIMIT 1
 ) p
), joined AS (
 SELECT * FROM base UNION ALL SELECT * FROM recent
), deltas AS (
 SELECT repo,tag,asset,day,GREATEST(download_count-lag(download_count) OVER (PARTITION BY repo,tag,asset ORDER BY day),0) AS delta FROM joined
)
SELECT repo,tag,asset,COALESCE(sum(delta) FILTER (WHERE day=$2),0),sum(delta)
 FROM deltas WHERE day>=$1 GROUP BY repo,tag,asset HAVING sum(delta)>0
 ORDER BY sum(delta) DESC,repo,tag,asset`

// collectGitHubReleaseDownloads 读取快照差分，首次快照没有可比较的基线，不计下载；元数据文件也不计入，超过行数上限的资产仍计入总数并设置截断标记。
func collectGitHubReleaseDownloads(rows pgx.Rows, summary *downloadsSummary) ([]downloadsRow, error) {
	defer rows.Close()
	channel := "github"
	result := []downloadsRow{}
	for rows.Next() {
		row := downloadsRow{Source: downloadsSourceGitHub, Channel: &channel}
		var asset string
		if err := rows.Scan(&row.Repo, &row.Tag, &asset, &row.Today, &row.Week); err != nil {
			return nil, err
		}
		if releaseMetadataAsset(asset) {
			continue
		}
		summary.Totals.GitHubToday += row.Today
		summary.Totals.GitHubWeek += row.Week
		if len(result) == downloadsRowLimit {
			summary.Truncated = true
			continue
		}
		row.Artifact = &asset
		row.Platform, row.Version = releaseTagPlatform(row.Tag, asset)
		result = append(result, row)
	}
	return result, rows.Err()
}

// releaseTag splits the configured tag convention "<platform>-v<version>", for example windows-v0.5.4.
var releaseTag = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9]*)-(v?[0-9][0-9A-Za-z.+_-]*)$`)

// releaseTagPlatforms are the tag prefixes that name a platform; any other prefix (a product name such as msime-v1.2.3) is not a platform, so such tags fall back to the installer extension.
var releaseTagPlatforms = map[string]string{"windows": "windows", "win": "windows", "macos": "macos", "mac": "macos", "osx": "macos", "linux": "linux", "android": "android", "ios": "ios", "harmony": "harmony", "harmonyos": "harmony", "ohos": "harmony"}

// releaseTagPlatform 优先按平台标签识别平台和版本，否则保留完整标签作为版本，并按安装包的最后一个扩展名识别平台；未知平台返回空字符串。
func releaseTagPlatform(tag, asset string) (string, string) {
	if m := releaseTag.FindStringSubmatch(tag); m != nil {
		if platform, ok := releaseTagPlatforms[strings.ToLower(m[1])]; ok {
			return platform, m[2]
		}
	}
	if extension := releaseAssetExtension(asset); extension != "" {
		switch extension {
		case ".exe", ".msi", ".msix", ".appx":
			return "windows", tag
		case ".dmg", ".pkg":
			return "macos", tag
		case ".deb", ".rpm", ".appimage", ".flatpak", ".snap":
			return "linux", tag
		case ".apk", ".aab":
			return "android", tag
		case ".ipa":
			return "ios", tag
		case ".hap", ".app":
			return "harmony", tag
		}
	}
	return "", tag
}

// releaseAssetExtension 只转换最后一个扩展名，避免为无关的文件名前缀分配小写副本。
func releaseAssetExtension(asset string) string {
	if dot := strings.LastIndexByte(asset, '.'); dot >= 0 {
		return strings.ToLower(asset[dot:])
	}
	return ""
}

// releaseMetadataAsset 识别校验和、签名和更新清单；这些下载属于更新检查或校验，不计作安装。
func releaseMetadataAsset(asset string) bool {
	switch releaseAssetExtension(asset) {
	case ".sha256", ".sha512", ".sha256sum", ".sig", ".asc", ".minisig", ".txt", ".yml", ".yaml", ".json", ".blockmap", ".zsync":
		return true
	}
	return false
}
