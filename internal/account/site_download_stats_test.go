package account

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestSiteDownloadStatsCumulativeMirrors(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	if _, err := db.pool.Exec(ctx, `TRUNCATE admin_events;
 INSERT INTO admin_events(id,kind,platform,version,channel,created_at) VALUES
 ('mirror-old-windows-01','download','Windows','1','cn-mirror',now()-interval '400 days'),
 ('mirror-new-windows-01','download','win','1','cn-mirror',now()),
 ('mirror-rust-windows-1','download','windows-rust','1','cn-mirror',now()),
 ('mirror-macos-0000001','download','darwin','1','cn-mirror',now()),
 ('mirror-harmony-00001','download','ohos','1','cn-mirror',now()),
 ('mirror-other-0000001','download','future','1','cn-mirror',now()),
 ('ignored-github-00001','download','windows','1','github',now()),
 ('ignored-no-channel-1','download','windows','1',NULL,now()),
 ('ignored-session-0001','session','windows','1','cn-mirror',now());`); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	Mount(mux, &Service{store: db})
	for _, token := range []string{"", "invalid-token"} {
		w := apiRequest(t, mux, "GET", siteDownloadStatsPath, "", token, 200)
		var stats SiteDownloadStats
		if err := json.Unmarshal(w.Body.Bytes(), &stats); err != nil {
			t.Fatal(err)
		}
		if stats.Total != 6 || stats.Platforms["windows"] != 2 || stats.Platforms["windowsRust"] != 1 || stats.Platforms["macos"] != 1 || stats.Platforms["harmony"] != 1 || stats.Platforms["other"] != 1 || len(stats.Platforms) != 5 {
			t.Fatal(w.Body.String())
		}
		if time.Since(stats.GeneratedAt) > time.Minute || w.Header().Get("Cache-Control") != "public, max-age=60" || w.Header().Get("Vary") != "Origin" {
			t.Fatal(stats, w.Header())
		}
		var sum int64
		for _, v := range stats.Platforms {
			sum += v
		}
		if sum != stats.Total {
			t.Fatal("累计值必须等于全部平台之和")
		}
	}
	if _, err := db.pool.Exec(ctx, `TRUNCATE admin_events`); err != nil {
		t.Fatal(err)
	}
	w := apiRequest(t, mux, "GET", siteDownloadStatsPath, "", "", 200)
	var stats SiteDownloadStats
	if err := json.Unmarshal(w.Body.Bytes(), &stats); err != nil || stats.Total != 0 || stats.Platforms == nil || len(stats.Platforms) != 0 {
		t.Fatal(w.Body.String(), err)
	}
	disabled := http.NewServeMux()
	Mount(disabled, nil)
	apiRequest(t, disabled, "GET", siteDownloadStatsPath, "", "", 503)
	apiRequest(t, mux, "POST", siteDownloadStatsPath, `{}`, "", 405)
	if !IsPath(siteDownloadStatsPath) {
		t.Fatal("公开统计不能要求设备令牌")
	}
	if scope, limit := accountRouteRate("GET " + siteDownloadStatsPath); scope != "feed-ip:" || limit != publicFeedRateLimit {
		t.Fatal(scope, limit)
	}
	db.pool.Close()
	apiRequest(t, mux, "GET", siteDownloadStatsPath, "", "", 503)
}
