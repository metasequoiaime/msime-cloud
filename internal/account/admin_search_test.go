package account

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestAdminSearchFindsEveryKind(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `TRUNCATE admin_crash_groups,admin_sensitive_words,admin_notices CASCADE`); err != nil {
		t.Fatal(err)
	}
	a := &Service{store: db}
	owner := complete(t, db, Identity{"email", "searcher@example.test"})
	if _, err := db.pool.Exec(ctx, `UPDATE auth_users SET display_name='Spring Artist' WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	seed := []string{
		`INSERT INTO community_skins(id,owner_id,name,design) VALUES('skin-spring',$1,'春日 Spring','{}')`,
		`INSERT INTO community_resources(id,owner_id,kind,name,content) VALUES('dict-spring',$1,'dictionary','Spring 词条','{}'),('reply-spring',$1,'reply','spring reply','{}')`,
		`INSERT INTO community_plugins(id,owner_id,kind,plugin_id,name,version,license,manifest,archive,request_sha256) VALUES('00000000-0000-4000-8000-000000000001',$1,'sound','spring.sound','Spring Sound','1.0','MIT','m','a',repeat('0',64))`,
	}
	for _, statement := range seed {
		if _, err := db.pool.Exec(ctx, statement, owner.User.ID); err != nil {
			t.Fatal(statement, err)
		}
	}
	insertCandidateSkin(t, db, "00000000-0000-4000-8000-000000000002", owner.User.ID, "Spring candidate")
	if _, err := db.pool.Exec(ctx, `INSERT INTO admin_crash_groups(signature,platform,version,title) VALUES('0123456789abcdef','ios','1.0','SpringBoard watchdog');
INSERT INTO admin_sensitive_words(pattern,category,level,created_by) VALUES('spring加V','ad','block','t');
INSERT INTO admin_notices(title,created_by) VALUES('Spring 版本公告','t'),('','t')`); err != nil {
		t.Fatal(err)
	}
	hits, err := a.AdminSearch(ctx, "  SPRING ", 20)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]AdminSearchHit{}
	for _, hit := range hits {
		got[hit.Kind] = hit
	}
	want := map[string]AdminSearchHit{
		"user":           {Kind: "user", ID: owner.User.ID, Title: "Spring Artist", Where: "用户账号", Target: "users"},
		"skin":           {Kind: "skin", ID: "skins/skin-spring", Title: "春日 Spring · 皮肤", Where: "社区审核", Target: "community"},
		"candidate-skin": {Kind: "candidate-skin", ID: "candidate-skins/00000000-0000-4000-8000-000000000002", Title: "Spring candidate · 候选皮肤", Where: "社区审核", Target: "community"},
		"plugin":         {Kind: "plugin", ID: "plugins/00000000-0000-4000-8000-000000000001", Title: "Spring Sound · 插件", Where: "社区审核", Target: "community"},
		"dictionary":     {Kind: "dictionary", ID: "dictionaries/dict-spring", Title: "Spring 词条 · 词库", Where: "社区审核", Target: "community"},
		"reply":          {Kind: "reply", ID: "replies/reply-spring", Title: "spring reply · 回复模板", Where: "社区审核", Target: "community"},
		"crash_group":    {Kind: "crash_group", ID: "0123456789abcdef", Title: "SpringBoard watchdog", Where: "崩溃上报", Target: "crash"},
		"sensitive_word": {Kind: "sensitive_word", Title: "spring加V · 敏感词", Where: "敏感词库", Target: "words"},
		"notice":         {Kind: "notice", Title: "Spring 版本公告", Where: "公告推送", Target: "notice"},
	}
	if len(hits) != len(want) {
		t.Fatalf("got %d hits %+v", len(hits), hits)
	}
	for kind, expected := range want {
		hit := got[kind]
		if expected.ID == "" {
			expected.ID = hit.ID
		}
		if hit != expected || hit.ID == "" {
			t.Errorf("%s: got %+v want %+v", kind, hit, expected)
		}
	}
	// An exact match ranks before substring matches, and the limit applies across kinds.
	exact, err := a.AdminSearch(ctx, "spring reply", 1)
	if err != nil || len(exact) != 1 || exact[0].Kind != "reply" {
		t.Fatalf("exact %+v %v", exact, err)
	}
	if byID, err := a.AdminSearch(ctx, owner.User.ID, 8); err != nil || len(byID) != 1 || byID[0].Kind != "user" {
		t.Fatalf("by id %+v %v", byID, err)
	}
	// A pasted community UUID matches whatever its case.
	if byID, err := a.AdminSearch(ctx, "00000000-0000-4000-8000-00000000000A", 8); err != nil || len(byID) != 0 {
		t.Fatalf("unknown uuid %+v %v", byID, err)
	}
	if byID, err := a.AdminSearch(ctx, "00000000-0000-4000-8000-000000000001", 8); err != nil || len(byID) != 1 || byID[0].Kind != "plugin" {
		t.Fatalf("plugin by id %+v %v", byID, err)
	}
	if _, err := db.pool.Exec(ctx, `UPDATE community_plugins SET id='0000000a-0000-4000-8000-000000000001' WHERE id='00000000-0000-4000-8000-000000000001'`); err != nil {
		t.Fatal(err)
	}
	if byID, err := a.AdminSearch(ctx, "0000000A-0000-4000-8000-000000000001", 8); err != nil || len(byID) != 1 || byID[0].ID != "plugins/0000000a-0000-4000-8000-000000000001" {
		t.Fatalf("uppercase plugin id %+v %v", byID, err)
	}
	// LIKE metacharacters are matched literally.
	for _, q := range []string{"%", "_", "\\"} {
		if hits, err := a.AdminSearch(ctx, q, 8); err != nil || len(hits) != 0 {
			t.Fatalf("%q matched %+v %v", q, hits, err)
		}
	}
	if hits, err := a.AdminSearch(ctx, "   ", 8); err != nil || len(hits) != 0 {
		t.Fatal(hits, err)
	}
	if _, err := a.AdminSearch(ctx, strings.Repeat("长", MaxAdminSearchQuery+1), 8); !errors.Is(err, ErrInvalid) {
		t.Fatal("oversized query accepted", err)
	}
}

func TestAdminSearchResultCapacity(t *testing.T) {
	const limit = 20
	samples := make([]AdminSearchHit, limit)
	for i := range samples {
		samples[i] = AdminSearchHit{ID: string(rune('a' + i))}
	}
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			hits := []AdminSearchHit{}
			for i := range limit {
				hits = appendAdminSearchHit(hits, samples[i], limit)
			}
			if len(hits) != limit || cap(hits) != limit {
				b.Fatalf("搜索结果长度=%d、容量=%d，期望均为 %d", len(hits), cap(hits), limit)
			}
		}
	})
	if result.AllocsPerOp() > 1 {
		t.Fatalf("20 条搜索结果分配 %d 次，超过一次结果切片分配", result.AllocsPerOp())
	}
}

func previousAdminSearchHits(samples []AdminSearchHit, limit int) []AdminSearchHit {
	var hits []AdminSearchHit
	for _, hit := range samples[:limit] {
		hits = append(hits, hit)
	}
	return hits
}

func BenchmarkAdminSearchResultCapacity(b *testing.B) {
	const limit = 20
	samples := make([]AdminSearchHit, limit)
	for i := range samples {
		samples[i] = AdminSearchHit{ID: string(rune('a' + i))}
	}
	for _, tc := range []struct {
		name string
		fn   func([]AdminSearchHit, int) []AdminSearchHit
	}{
		{"previous", previousAdminSearchHits},
		{"current", func(samples []AdminSearchHit, limit int) []AdminSearchHit {
			var hits []AdminSearchHit
			for _, hit := range samples[:limit] {
				hits = appendAdminSearchHit(hits, hit, limit)
			}
			return hits
		}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if got := tc.fn(samples, limit); len(got) != limit {
					b.Fatal("搜索结果数量变化")
				}
			}
		})
	}
}
