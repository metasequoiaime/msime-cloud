package account

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestTypingUploadValidation(t *testing.T) {
	today := time.Date(2026, 10, 17, 23, 30, 0, 0, time.UTC)
	hours := make([]int64, 24)
	hours[22] = 900
	ok := typingUpload{Device: "desk-1", Platform: "windows", Days: []TypingDay{
		{Day: "2026-10-17", Characters: 983, ActiveSeconds: 1260, Hours: hours, Categories: map[string]int64{"han": 900, "latin": 83}, Sources: map[string]int64{"quanpin": 900, "voice": 83}},
		{Day: "2026-08-17", Characters: 1},
		{Day: "2026-10-18"},
	}, Selections: &TypingSelections{Ranks: []int64{800, 150, 30}, Beyond: 2}}
	if !validTypingUpload(ok, today) {
		t.Fatal("valid upload rejected")
	}
	mutate := func(f func(*typingUpload)) typingUpload {
		v := ok
		v.Days = append([]TypingDay(nil), ok.Days...)
		selections := *ok.Selections
		v.Selections = &selections
		f(&v)
		return v
	}
	tooManyKeys := map[string]int64{}
	for i := range typingStatisticsMaxKeys + 1 {
		tooManyKeys[fmt.Sprintf("k%d", i)] = 1
	}
	for name, v := range map[string]typingUpload{
		"device empty":        mutate(func(v *typingUpload) { v.Device = "" }),
		"device characters":   mutate(func(v *typingUpload) { v.Device = "a b" }),
		"device too long":     mutate(func(v *typingUpload) { v.Device = strings.Repeat("a", 65) }),
		"platform":            mutate(func(v *typingUpload) { v.Platform = "beos" }),
		"no days":             mutate(func(v *typingUpload) { v.Days = nil }),
		"too many days":       mutate(func(v *typingUpload) { v.Days = make([]TypingDay, typingStatisticsUploadDays+1) }),
		"bad date":            mutate(func(v *typingUpload) { v.Days[1].Day = "2026-13-01" }),
		"duplicate date":      mutate(func(v *typingUpload) { v.Days[1].Day = "2026-10-17" }),
		"too old":             mutate(func(v *typingUpload) { v.Days[1].Day = "2026-08-16" }),
		"future":              mutate(func(v *typingUpload) { v.Days[2].Day = "2026-10-19" }),
		"negative":            mutate(func(v *typingUpload) { v.Days[0].Characters = -1 }),
		"huge":                mutate(func(v *typingUpload) { v.Days[0].Characters = typingStatisticsMaxCount + 1 }),
		"active over a day":   mutate(func(v *typingUpload) { v.Days[0].ActiveSeconds = 24*3600 + 1 }),
		"hours length":        mutate(func(v *typingUpload) { v.Days[0].Hours = []int64{1} }),
		"hour negative":       mutate(func(v *typingUpload) { v.Days[0].Hours = append([]int64{-1}, hours[1:]...) }),
		"category key":        mutate(func(v *typingUpload) { v.Days[0].Categories = map[string]int64{"汉字": 1} }),
		"source value":        mutate(func(v *typingUpload) { v.Days[0].Sources = map[string]int64{"quanpin": -1} }),
		"too many keys":       mutate(func(v *typingUpload) { v.Days[0].Sources = tooManyKeys }),
		"ranks missing":       mutate(func(v *typingUpload) { v.Selections.Ranks = nil }),
		"too many ranks":      mutate(func(v *typingUpload) { v.Selections.Ranks = make([]int64, typingStatisticsMaxRanks+1) }),
		"rank negative":       mutate(func(v *typingUpload) { v.Selections.Ranks = []int64{-1} }),
		"beyond out of range": mutate(func(v *typingUpload) { v.Selections.Beyond = -1 }),
	} {
		if validTypingUpload(v, today) {
			t.Errorf("%s accepted", name)
		}
	}
	noSelections := mutate(func(v *typingUpload) { v.Selections = nil; v.Platform = "" })
	if !validTypingUpload(noSelections, today) {
		t.Fatal("upload without selections or platform rejected")
	}
}

// 上传按设备和日期整行替换，不带位次时保留上次的；第 9 台设备挤掉最久没上传的那台；DELETE 清空本人的全部设备，别人的不受影响；导出包里有打字统计和绑定。
func TestTypingStatisticsUploadAndDelete(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	one := complete(t, db, Identity{"email", "typing-one@example.test"})
	two := complete(t, db, Identity{"email", "typing-two@example.test"})
	mux := http.NewServeMux()
	a := &Service{store: db}
	Mount(mux, a)
	path := "/v1/users/me/typing-statistics"
	today := time.Now().UTC().Format(time.DateOnly)
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format(time.DateOnly)
	apiRequest(t, mux, "PUT", path, `{"device":"d","days":[{"day":"`+today+`","characters":1,"active_seconds":1}]}`, "", 401)
	apiRequest(t, mux, "PUT", path, `{"device":"d","days":[]}`, one.AccessToken, 400)
	apiRequest(t, mux, "PUT", path, `{"device":"d","days":[{"day":"`+today+`","characters":1,"active_seconds":1}],"extra":1}`, one.AccessToken, 400)
	apiRequest(t, mux, "PUT", path, `{"device":"desk","platform":"windows","days":[{"day":"`+today+`","characters":100,"active_seconds":60,"sources":{"quanpin":100}},{"day":"`+yesterday+`","characters":50,"active_seconds":30}],"selections":{"ranks":[8,2],"beyond":1}}`, one.AccessToken, 204)
	// 同一天重传整行替换；不带位次时保留上次的。
	apiRequest(t, mux, "PUT", path, `{"device":"desk","platform":"windows","days":[{"day":"`+today+`","characters":120,"active_seconds":70,"hours":[0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,120,0],"sources":{"wubi":120}}]}`, one.AccessToken, 204)
	apiRequest(t, mux, "PUT", path, `{"device":"phone","platform":"android","days":[{"day":"`+today+`","characters":30,"active_seconds":20,"hours":[0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,10,20],"sources":{"wubi":10,"voice":20}}],"selections":{"ranks":[1,1,1],"beyond":0}}`, one.AccessToken, 204)
	apiRequest(t, mux, "PUT", path, `{"device":"desk","days":[{"day":"`+today+`","characters":5,"active_seconds":5}]}`, two.AccessToken, 204)

	code, err := a.CreateQQLinkCode(ctx, "G/M1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RedeemQQLinkCode(ctx, one.User.ID, code); err != nil {
		t.Fatal(err)
	}
	report, err := a.QQTypingReport(ctx, "G/M1", time.Now().UTC().AddDate(0, 0, -7))
	if err != nil {
		t.Fatal(err)
	}
	if !report.Linked || report.Devices != 2 || len(report.Days) != 2 || report.Days[0].Day != yesterday || report.Days[0].Characters != 50 {
		t.Fatalf("%+v", report)
	}
	merged := report.Days[1]
	if merged.Characters != 150 || merged.ActiveSeconds != 90 || merged.Hours[22] != 130 || merged.Hours[23] != 20 || merged.Sources["wubi"] != 130 || merged.Sources["voice"] != 20 || merged.Sources["quanpin"] != 0 {
		t.Fatalf("%+v", merged)
	}
	if fmt.Sprint(report.Selections.Ranks) != "[9 3 1]" || report.Selections.Beyond != 1 {
		t.Fatalf("%+v", report.Selections)
	}
	// 同一天只有一台设备有每小时的数时用它的；两台都没有时仍然没有。
	if got := addTypingDay(TypingDay{Day: today, Characters: 1}, TypingDay{Day: today, Characters: 2}); got.Hours != nil || got.Characters != 3 {
		t.Fatalf("%+v", got)
	}

	// 导出包里有本人的打字统计和绑定，没有别人的。
	w := userDataRequest(mux, "GET", "/v1/users/me/data/export", one.AccessToken, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	archive, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var exported struct {
		Devices []map[string]any `json:"devices"`
		Days    []map[string]any `json:"days"`
		QQLinks []map[string]any `json:"qq_links"`
	}
	for _, f := range archive.File {
		if f.Name != "typing_statistics.json" {
			continue
		}
		r, _ := f.Open()
		b, _ := io.ReadAll(r)
		r.Close()
		if err := json.Unmarshal(b, &exported); err != nil {
			t.Fatal(err, string(b))
		}
	}
	if len(exported.Devices) != 2 || len(exported.Days) != 3 || len(exported.QQLinks) != 1 {
		t.Fatalf("%+v", exported)
	}

	// 第 9 台设备挤掉最久没上传的那台（desk），它的每日汇总随外键删除。
	if _, err := db.pool.Exec(ctx, `UPDATE typing_statistics_devices SET updated_at=now()-interval '1 day' WHERE user_id=$1 AND device='desk'`, one.User.ID); err != nil {
		t.Fatal(err)
	}
	for i := range typingStatisticsDevices - 1 {
		apiRequest(t, mux, "PUT", path, fmt.Sprintf(`{"device":"extra-%d","days":[{"day":"%s","characters":1,"active_seconds":1}]}`, i, today), one.AccessToken, 204)
	}
	var devices, deskDays int
	if err := db.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM typing_statistics_devices WHERE user_id=$1),(SELECT count(*) FROM typing_statistics_days WHERE user_id=$1 AND device='desk')`, one.User.ID).Scan(&devices, &deskDays); err != nil {
		t.Fatal(err)
	}
	if devices != typingStatisticsDevices || deskDays != 0 {
		t.Fatal(devices, deskDays)
	}
	// 已登记的设备再上传不会挤掉别的。
	apiRequest(t, mux, "PUT", path, `{"device":"phone","days":[{"day":"`+today+`","characters":31,"active_seconds":20}]}`, one.AccessToken, 204)
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM typing_statistics_devices WHERE user_id=$1`, one.User.ID).Scan(&devices); err != nil || devices != typingStatisticsDevices {
		t.Fatal(devices, err)
	}

	apiRequest(t, mux, "DELETE", path, "", one.AccessToken, 204)
	report, err = a.QQTypingReport(ctx, "G/M1", time.Now().UTC().AddDate(0, 0, -7))
	if err != nil || !report.Linked || report.Devices != 0 || len(report.Days) != 0 {
		t.Fatalf("%+v %v", report, err)
	}
	var others int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM typing_statistics_days WHERE user_id=$1`, two.User.ID).Scan(&others); err != nil || others != 1 {
		t.Fatal(others, err)
	}

	// 被封禁的账号写不进来。
	if _, err := db.pool.Exec(ctx, `UPDATE auth_users SET banned_at=now() WHERE id=$1`, two.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.PutTypingStatistics(ctx, two.User.ID, typingUpload{Device: "desk", Days: []TypingDay{{Day: today}}}); err == nil {
		t.Fatal("banned account wrote typing statistics")
	}
}

// 每小时上传次数有上限。
func TestTypingStatisticsUploadRate(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"email", "typing-rate@example.test"})
	mux := http.NewServeMux()
	Mount(mux, &Service{store: db})
	body := `{"device":"d","days":[{"day":"` + time.Now().UTC().Format(time.DateOnly) + `","characters":1,"active_seconds":1}]}`
	for range typingStatisticsUploadsPerHour {
		apiRequest(t, mux, "PUT", "/v1/users/me/typing-statistics", body, user.AccessToken, 204)
	}
	apiRequest(t, mux, "PUT", "/v1/users/me/typing-statistics", body, user.AccessToken, 429)
}

// 绑定码：一次性、过期作废、再要一次时旧码作废；改绑到新账号；账号绑定数和提交次数有上限；GET 列出、DELETE 解绑；没绑定和被封禁的账号机器人看不到。
func TestQQLinks(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `TRUNCATE qq_link_codes`); err != nil {
		t.Fatal(err)
	}
	one := complete(t, db, Identity{"email", "qq-link-one@example.test"})
	two := complete(t, db, Identity{"email", "qq-link-two@example.test"})
	if _, err := db.pool.Exec(ctx, `UPDATE auth_users SET display_name='一号' WHERE id=$1`, one.User.ID); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	a := &Service{store: db}
	Mount(mux, a)
	path := "/v1/users/me/qq-links"
	since := time.Now().UTC().AddDate(0, 0, -7)

	if report, err := a.QQTypingReport(ctx, "G/M1", since); err != nil || report.Linked {
		t.Fatal(report, err)
	}
	stale, err := a.CreateQQLinkCode(ctx, "G/M1")
	if err != nil {
		t.Fatal(err)
	}
	code, err := a.CreateQQLinkCode(ctx, "G/M1")
	if err != nil || code == stale || len(code) != 8 {
		t.Fatal(code, stale, err)
	}
	apiRequest(t, mux, "POST", path, `{"code":"`+code+`"}`, "", 401)
	apiRequest(t, mux, "POST", path, `{"code":"1234"}`, one.AccessToken, 400)
	apiRequest(t, mux, "POST", path, `{"code":"`+stale+`"}`, one.AccessToken, 404)
	var link QQLink
	if err := json.Unmarshal(apiRequest(t, mux, "POST", path, `{"code":"`+code+`"}`, one.AccessToken, 201).Body.Bytes(), &link); err != nil || link.ID == "" {
		t.Fatal(link, err)
	}
	apiRequest(t, mux, "POST", path, `{"code":"`+code+`"}`, two.AccessToken, 404)
	report, err := a.QQTypingReport(ctx, "G/M1", since)
	if err != nil || !report.Linked || report.DisplayName != "一号" || report.Devices != 0 {
		t.Fatal(report, err)
	}
	var listed struct{ Links []QQLink }
	if err := json.Unmarshal(apiRequest(t, mux, "GET", path, "", one.AccessToken, 200).Body.Bytes(), &listed); err != nil || len(listed.Links) != 1 || listed.Links[0].ID != link.ID {
		t.Fatal(listed, err)
	}
	if err := json.Unmarshal(apiRequest(t, mux, "GET", path, "", two.AccessToken, 200).Body.Bytes(), &listed); err != nil || listed.Links == nil || len(listed.Links) != 0 {
		t.Fatal(listed, err)
	}

	// 过期的码不能用。
	expired, err := a.CreateQQLinkCode(ctx, "G/M2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `UPDATE qq_link_codes SET expires_at=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	apiRequest(t, mux, "POST", path, `{"code":"`+expired+`"}`, two.AccessToken, 404)
	db.Prune(ctx)
	var codes int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM qq_link_codes`).Scan(&codes); err != nil || codes != 0 {
		t.Fatal(codes, err)
	}

	// 同一个群友改绑到另一个账号。
	again, err := a.CreateQQLinkCode(ctx, "G/M1")
	if err != nil {
		t.Fatal(err)
	}
	apiRequest(t, mux, "POST", path, `{"code":"`+again+`"}`, two.AccessToken, 201)
	if report, err := a.QQTypingReport(ctx, "G/M1", since); err != nil || report.DisplayName == "一号" {
		t.Fatal(report, err)
	}
	apiRequest(t, mux, "DELETE", path+"/"+link.ID, "", one.AccessToken, 404)
	apiRequest(t, mux, "DELETE", path+"/abc", "", one.AccessToken, 404)

	// 解绑。
	if err := json.Unmarshal(apiRequest(t, mux, "GET", path, "", two.AccessToken, 200).Body.Bytes(), &listed); err != nil || len(listed.Links) != 1 {
		t.Fatal(listed, err)
	}
	apiRequest(t, mux, "DELETE", path+"/"+listed.Links[0].ID, "", two.AccessToken, 204)
	if report, err := a.QQTypingReport(ctx, "G/M1", since); err != nil || report.Linked {
		t.Fatal(report, err)
	}

	// 机器人那边解绑。
	again, _ = a.CreateQQLinkCode(ctx, "G/M1")
	if _, err := db.RedeemQQLinkCode(ctx, one.User.ID, again); err != nil {
		t.Fatal(err)
	}
	if removed, err := a.UnlinkQQ(ctx, "G/M1"); err != nil || !removed {
		t.Fatal(removed, err)
	}
	if removed, err := a.UnlinkQQ(ctx, "G/M1"); err != nil || removed {
		t.Fatal(removed, err)
	}

	// 一个账号最多绑 qqLinksPerUser 个群友；到上限时码不作废。
	for i := range qqLinksPerUser {
		c, _ := a.CreateQQLinkCode(ctx, fmt.Sprintf("G/L%d", i))
		if _, err := db.RedeemQQLinkCode(ctx, one.User.ID, c); err != nil {
			t.Fatal(i, err)
		}
	}
	over, _ := a.CreateQQLinkCode(ctx, "G/over")
	apiRequest(t, mux, "POST", path, `{"code":"`+over+`"}`, one.AccessToken, 409)
	apiRequest(t, mux, "POST", path, `{"code":"`+over+`"}`, two.AccessToken, 201)

	// 被封禁的账号：机器人当作没绑定，也提交不了绑定码。
	if _, err := db.pool.Exec(ctx, `UPDATE auth_users SET banned_at=now() WHERE id=$1`, one.User.ID); err != nil {
		t.Fatal(err)
	}
	if report, err := a.QQTypingReport(ctx, "G/L0", since); err != nil || report.Linked {
		t.Fatal(report, err)
	}
	banned, _ := a.CreateQQLinkCode(ctx, "G/banned")
	if _, err := db.RedeemQQLinkCode(ctx, one.User.ID, banned); err == nil {
		t.Fatal("banned account redeemed a code")
	}
}

// 提交绑定码的次数有上限，猜错的也算。
func TestQQLinkAttemptRate(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"email", "qq-link-rate@example.test"})
	mux := http.NewServeMux()
	Mount(mux, &Service{store: db})
	for range qqLinkAttemptsPerHour {
		apiRequest(t, mux, "POST", "/v1/users/me/qq-links", `{"code":"00000000"}`, user.AccessToken, 404)
	}
	apiRequest(t, mux, "POST", "/v1/users/me/qq-links", `{"code":"00000000"}`, user.AccessToken, 429)
}
