package account

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"
	"time"
)

// The site and OpenAPI both depend on these exact keys, so pin the wire shape without needing a database.
func TestCommunityStatsWireShape(t *testing.T) {
	raw, err := json.Marshal(CommunityStats{Skins: 1, SkinDownloads: 2, Dictionaries: 3, Replies: 4, ResourceSaves: 5, GeneratedAt: time.Date(2026, 9, 30, 1, 2, 3, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if want := []string{"dictionaries", "generated_at", "replies", "resource_saves", "skin_downloads", "skins"}; !reflect.DeepEqual(keys, want) {
		t.Fatal(keys)
	}
	if fields["generated_at"] != "2026-09-30T01:02:03Z" || fields["skins"] != float64(1) || fields["resource_saves"] != float64(5) {
		t.Fatal(string(raw))
	}
}

func TestCommunityStatsCountsPublicContentAnonymously(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	a := &Service{store: db}
	mux := http.NewServeMux()
	Mount(mux, a)
	stats := func() (CommunityStats, *httptest.ResponseRecorder) {
		t.Helper()
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/community/stats", nil))
		var s CommunityStats
		if w.Code == 200 {
			d := json.NewDecoder(w.Body)
			d.DisallowUnknownFields()
			if err := d.Decode(&s); err != nil {
				t.Fatal(err)
			}
		}
		return s, w
	}
	before := time.Now().Add(-time.Minute)
	empty, w := stats()
	if w.Code != 200 || empty != (CommunityStats{GeneratedAt: empty.GeneratedAt}) {
		t.Fatal(w.Code, w.Body.String(), empty)
	}
	if empty.GeneratedAt.Before(before) || empty.GeneratedAt.Location() != time.UTC {
		t.Fatal("generated_at is not the current UTC database time", empty.GeneratedAt)
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Type") != "application/json; charset=utf-8" || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal(w.Header())
	}
	owner := complete(t, db, Identity{"apple", "stats-owner"})
	reader := complete(t, db, Identity{"apple", "stats-reader"})
	leaver := complete(t, db, Identity{"apple", "stats-leaver"})
	for _, fixture := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO community_skins(id,owner_id,name,design) VALUES('stats-skin-1',$1,'One','{}'),('stats-skin-2',$1,'Two','{}')`, []any{owner.User.ID}},
		{`INSERT INTO community_skin_downloads(skin_id,user_id) VALUES('stats-skin-1',$1),('stats-skin-1',$2),('stats-skin-2',$1)`, []any{reader.User.ID, leaver.User.ID}},
		{`INSERT INTO community_resources(id,owner_id,kind,name,content) VALUES('stats-dict-1',$1,'dictionary','Words','{"entries":[]}'),('stats-dict-2',$2,'dictionary','More','{"entries":[]}'),('stats-reply-1',$1,'reply','Reply','{"prompt":"hi"}')`, []any{owner.User.ID, leaver.User.ID}},
		{`INSERT INTO community_resource_saves(resource_id,user_id) VALUES('stats-dict-1',$1),('stats-reply-1',$1),('stats-reply-1',$2)`, []any{reader.User.ID, leaver.User.ID}},
	} {
		if _, err := db.pool.Exec(ctx, fixture.sql, fixture.args...); err != nil {
			t.Fatal(err)
		}
	}
	got, w := stats()
	if w.Code != 200 || got != (CommunityStats{Skins: 2, SkinDownloads: 3, Dictionaries: 2, Replies: 1, ResourceSaves: 3, GeneratedAt: got.GeneratedAt}) {
		t.Fatal(w.Code, w.Body.String())
	}
	if got.GeneratedAt.Before(empty.GeneratedAt) {
		t.Fatal("generated_at moved backwards", empty.GeneratedAt, got.GeneratedAt)
	}
	// Deleting an account removes its works and interactions, so the public totals must drop with it.
	if err := db.DeleteUser(ctx, leaver.User.ID); err != nil {
		t.Fatal(err)
	}
	if got, w = stats(); w.Code != 200 || got != (CommunityStats{Skins: 2, SkinDownloads: 2, Dictionaries: 1, Replies: 1, ResourceSaves: 2, GeneratedAt: got.GeneratedAt}) {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, "/v1/community/stats", nil))
		if w.Code != 405 {
			t.Fatal(method, w.Code)
		}
	}
	db.Close()
	// The mounted route fails in the shared rate limiter first; call the handler directly as well so its own query failure is exercised.
	direct := httptest.NewRecorder()
	a.communityStats(direct, httptest.NewRequest("GET", "/v1/community/stats", nil))
	_, mounted := stats()
	for name, w := range map[string]*httptest.ResponseRecorder{"mounted": mounted, "handler": direct} {
		var failure struct {
			Error struct{ Code string } `json:"error"`
		}
		if w.Code != 503 || w.Header().Get("Cache-Control") != "no-store" || json.Unmarshal(w.Body.Bytes(), &failure) != nil || failure.Error.Code != "auth_unavailable" {
			t.Fatal(name, "unavailable database", w.Code, w.Body.String())
		}
	}
}

func TestCommunityStatsExcludesRemovedSkinDownloads(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "stats-removed-owner@example.test"})
	reader := complete(t, db, Identity{"email", "stats-removed-reader@example.test"})
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_skins(id,owner_id,name,design,moderation) VALUES('stats-removed-skin',$1,'待下架','{}','removed')`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_skin_downloads(skin_id,user_id) VALUES('stats-removed-skin',$1)`, reader.User.ID); err != nil {
		t.Fatal(err)
	}
	var stats CommunityStats
	if err := db.pool.QueryRow(t.Context(), communityStatsQuery).Scan(&stats.Skins, &stats.SkinDownloads, &stats.Dictionaries, &stats.Replies, &stats.ResourceSaves, &stats.GeneratedAt); err != nil {
		t.Fatal(err)
	}
	if stats.Skins != 0 || stats.SkinDownloads != 0 {
		t.Fatalf("已下架皮肤及其下载不应出现在公共统计：%+v", stats)
	}
}
