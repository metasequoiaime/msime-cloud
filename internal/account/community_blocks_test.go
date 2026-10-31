package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// blockMux 挂载账号路由，并像 server 包一样单独挂上屏蔽的三条路由。
func blockMux(a *Service) *http.ServeMux {
	mux := http.NewServeMux()
	Mount(mux, a)
	for pattern, method := range map[string]func(*Service, http.ResponseWriter, *http.Request){
		"POST /v1/community/blocks":        (*Service).CommunityBlockCreate,
		"GET /v1/community/blocks":         (*Service).CommunityBlockList,
		"DELETE /v1/community/blocks/{id}": (*Service).CommunityBlockDelete,
	} {
		mux.HandleFunc(pattern, Route(a, pattern, method))
	}
	return mux
}

func blockCreate(t *testing.T, mux http.Handler, token, kind, item string, status int) CommunityBlock {
	t.Helper()
	w := apiRequest(t, mux, "POST", "/v1/community/blocks", `{"kind":"`+kind+`","item_id":"`+item+`"}`, token, status)
	var body struct {
		Block *CommunityBlock `json:"block"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Block == nil || !canonicalUUID(body.Block.ID) || body.Block.CreatedAt.IsZero() {
		t.Fatal("屏蔽响应形状错误：", w.Body.String())
	}
	return *body.Block
}

func blockList(t *testing.T, mux http.Handler, token string) []CommunityBlock {
	t.Helper()
	w := apiRequest(t, mux, "GET", "/v1/community/blocks", "", token, 200)
	var body struct {
		Items []CommunityBlock `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Items == nil {
		t.Fatal("屏蔽列表形状错误：", w.Body.String())
	}
	return body.Items
}

func TestCommunityBlockCreateListDelete(t *testing.T) {
	db, _, owner, reader, call := moderationFixture(t)
	ctx := context.Background()
	mux := blockMux(&Service{store: db})
	if _, err := db.pool.Exec(ctx, `UPDATE auth_users SET display_name='春日作者' WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	body := `{"kind":"skins","item_id":"skin-a"}`
	apiRequest(t, mux, "POST", "/v1/community/blocks", body, "", 401)
	apiRequest(t, mux, "GET", "/v1/community/blocks", "", "", 401)
	apiRequest(t, mux, "DELETE", "/v1/community/blocks/00000000-0000-4000-8000-000000000000", "", "", 401)
	for _, tc := range []struct {
		body, code string
		status     int
	}{
		{`{"kind":"users","item_id":"skin-a"}`, "invalid_block_kind", 400},
		{`{"kind":"","item_id":"skin-a"}`, "invalid_block_kind", 400},
		{`{"kind":"skins","item_id":""}`, "invalid_id", 400},
		{`{"kind":"skins","item_id":"a/b"}`, "invalid_id", 400},
		{`{"kind":"skins","item_id":"` + strings.Repeat("a", 129) + `"}`, "invalid_id", 400},
		{`{"kind":"skins","item_id":"skin-a","extra":1}`, "invalid_json", 400},
		{`{"kind":"skins","item_id":"skin-missing"}`, "item_not_found", 404},
		// kind 决定查哪张表：回复模板的 ID 当作词库查不到。
		{`{"kind":"dictionaries","item_id":"reply-a"}`, "item_not_found", 404},
		{body, "cannot_block_self", 400},
	} {
		token := reader.AccessToken
		if tc.code == "cannot_block_self" {
			token = owner.AccessToken
		}
		if w := apiRequest(t, mux, "POST", "/v1/community/blocks", tc.body, token, tc.status); !strings.Contains(w.Body.String(), `"`+tc.code+`"`) {
			t.Fatal(tc.body, w.Body.String())
		}
	}
	// 新建返回 201，响应只有屏蔽记录的 ID 和作者显示名，不含作者的用户 ID。
	w := apiRequest(t, mux, "POST", "/v1/community/blocks", body, reader.AccessToken, 201)
	if strings.Contains(w.Body.String(), owner.User.ID) {
		t.Fatal("响应泄露了被屏蔽用户的 ID：", w.Body.String())
	}
	var created struct {
		Block CommunityBlock `json:"block"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.Block.Name != "春日作者" || !canonicalUUID(created.Block.ID) || time.Since(created.Block.CreatedAt) > time.Minute {
		t.Fatal(w.Body.String())
	}
	// 经同一作者的另一件作品再屏蔽一次，返回 200 和原记录，不新增。
	again := blockCreate(t, mux, reader.AccessToken, "replies", "reply-a", 200)
	if again.ID != created.Block.ID || !again.CreatedAt.Equal(created.Block.CreatedAt) {
		t.Fatal("重复屏蔽没有返回原记录：", again, created.Block)
	}
	var rows int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM community_blocks WHERE user_id=$1`, reader.User.ID).Scan(&rows); err != nil || rows != 1 {
		t.Fatal(rows, err)
	}
	// 显示名在读取时计算：作者改名后列表随之更新；清空昵称时退回默认名。
	if _, err := db.pool.Exec(ctx, `UPDATE auth_users SET display_name='  ' WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	items := blockList(t, mux, reader.AccessToken)
	if len(items) != 1 || items[0].ID != created.Block.ID || items[0].Name != "水杉小鹿·"+strings.ToUpper(owner.User.ID[:6]) || !items[0].CreatedAt.Equal(created.Block.CreatedAt) {
		t.Fatal("屏蔽列表错误：", items)
	}
	if got := blockList(t, mux, owner.AccessToken); len(got) != 0 {
		t.Fatal("别人的屏蔽出现在作者的列表里：", got)
	}
	// 已下架作品和私有候选皮肤不可见，与举报一样返回 404。
	if w := call("POST", "/api/actions", `{"action":"remove_content","section":"skins","id":"skin-b","reason":"内容低俗"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	other := complete(t, db, Identity{"anonymous", "block-other-device"})
	apiRequest(t, mux, "POST", "/v1/community/blocks", `{"kind":"skins","item_id":"skin-b"}`, other.AccessToken, 404)
	private := "b1334455-1234-4234-8234-0000000000b1"
	insertCandidateSkin(t, db, private, owner.User.ID, "Private")
	if _, err := db.pool.Exec(ctx, `UPDATE community_candidate_skins SET visibility='private' WHERE id=$1`, private); err != nil {
		t.Fatal(err)
	}
	apiRequest(t, mux, "POST", "/v1/community/blocks", `{"kind":"candidate-skins","item_id":"`+private+`"}`, other.AccessToken, 404)
	// 待审的脚本插件对作者以外的人不存在。
	script := "b1334455-1234-4234-8234-0000000000b2"
	if _, err := db.pool.Exec(ctx, `INSERT INTO community_plugins(id,owner_id,kind,plugin_id,name,version,license,manifest,archive,request_sha256,moderation) VALUES($1,$2,'script','block.script','脚本','1.0','MIT','合成清单','合成归档',repeat('c',64),'pending')`, script, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	apiRequest(t, mux, "POST", "/v1/community/blocks", `{"kind":"plugins","item_id":"`+script+`"}`, other.AccessToken, 404)
	// 设备的匿名账号也能屏蔽。
	anonymousBlock := blockCreate(t, mux, other.AccessToken, "dictionaries", "dict-a", 201)

	// 取消屏蔽：格式不对的 ID 返回 400；别人的记录不受影响；本人的删除后重复删除仍然成功。
	for _, id := range []string{"missing", "00000000-0000-4000-8000-00000000000", "00000000-0000-4000-8000-00000000000g", "00000000+0000-4000-8000-000000000000"} {
		if w := apiRequest(t, mux, "DELETE", "/v1/community/blocks/"+id, "", reader.AccessToken, 400); !strings.Contains(w.Body.String(), `"invalid_id"`) {
			t.Fatal(id, w.Body.String())
		}
	}
	if w := apiRequest(t, mux, "DELETE", "/v1/community/blocks/"+anonymousBlock.ID, "", reader.AccessToken, 200); strings.TrimSpace(w.Body.String()) != `{"blocked":false}` {
		t.Fatal(w.Body.String())
	}
	if got := blockList(t, mux, other.AccessToken); len(got) != 1 || got[0].ID != anonymousBlock.ID {
		t.Fatal("删除了别人的屏蔽：", got)
	}
	for range 2 {
		if w := apiRequest(t, mux, "DELETE", "/v1/community/blocks/"+strings.ToUpper(created.Block.ID), "", reader.AccessToken, 200); strings.TrimSpace(w.Body.String()) != `{"blocked":false}` {
			t.Fatal(w.Body.String())
		}
	}
	if got := blockList(t, mux, reader.AccessToken); len(got) != 0 {
		t.Fatal("取消屏蔽后列表不为空：", got)
	}
	// 取消后可以重新屏蔽，得到一条新记录。
	if again := blockCreate(t, mux, reader.AccessToken, "skins", "skin-a", 201); again.ID == created.Block.ID {
		t.Fatal("重新屏蔽复用了已删除的记录 ID")
	}
	// 封禁账号不能新建屏蔽。
	if _, err := db.pool.Exec(ctx, `UPDATE auth_users SET banned_at=now() WHERE id=$1`, other.User.ID); err != nil {
		t.Fatal(err)
	}
	if w := apiRequest(t, mux, "POST", "/v1/community/blocks", body, other.AccessToken, 403); !strings.Contains(w.Body.String(), "account_banned") {
		t.Fatal(w.Body.String())
	}
}

func TestCommunityBlockLimitAndRate(t *testing.T) {
	db, _, owner, reader, _ := moderationFixture(t)
	ctx := context.Background()
	mux := blockMux(&Service{store: db})
	// 预先屏蔽满 communityBlockLimit 位合成作者，再屏蔽一位新作者返回 409。
	if _, err := db.pool.Exec(ctx, `WITH authors AS (
 INSERT INTO auth_users(id) SELECT 'block-limit-'||n FROM generate_series(1,$2::int) n RETURNING id
)
INSERT INTO community_blocks(id,user_id,blocked_id,created_at) SELECT md5(id)::uuid::text,$1,id,now()-interval '1 hour' FROM authors`, reader.User.ID, communityBlockLimit); err != nil {
		t.Fatal(err)
	}
	if w := apiRequest(t, mux, "POST", "/v1/community/blocks", `{"kind":"skins","item_id":"skin-a"}`, reader.AccessToken, 409); !strings.Contains(w.Body.String(), `"block_limit_reached"`) {
		t.Fatal(w.Body.String())
	}
	// 列表一次返回全部，新的在前。
	items := blockList(t, mux, reader.AccessToken)
	if len(items) != communityBlockLimit {
		t.Fatal("屏蔽列表条数：", len(items))
	}
	for i := 1; i < len(items); i++ {
		if items[i-1].CreatedAt.Before(items[i].CreatedAt) {
			t.Fatal("屏蔽列表没有按时间倒序")
		}
	}
	// 已屏蔽的作者在满额时仍然按幂等返回原记录，取消一条后可以屏蔽新作者。
	if _, err := db.pool.Exec(ctx, `UPDATE community_skins SET owner_id='block-limit-1' WHERE id='skin-b'`); err != nil {
		t.Fatal(err)
	}
	existing := blockCreate(t, mux, reader.AccessToken, "skins", "skin-b", 200)
	apiRequest(t, mux, "DELETE", "/v1/community/blocks/"+existing.ID, "", reader.AccessToken, 200)
	if got := blockCreate(t, mux, reader.AccessToken, "skins", "skin-a", 201); got.Name == "" {
		t.Fatal(got)
	}
	newest := blockList(t, mux, reader.AccessToken)
	if len(newest) != communityBlockLimit || newest[0].Name != "水杉小鹿·"+strings.ToUpper(owner.User.ID[:6]) {
		t.Fatal("新屏蔽没有排在最前：", newest[0])
	}
	// 新建屏蔽按账号限流。
	for range communityBlocksPerHour + 1 {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, jsonRequest("POST", "/v1/community/blocks", `{"kind":"skins","item_id":"skin-a"}`, owner.AccessToken))
		if w.Code == 429 {
			return
		}
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	t.Fatal("屏蔽没有按账号限流")
}

// 屏蔽作者后，四个社区目录在 mine 以外的范围里都不再给屏蔽者列出该作者的作品；别人、未登录访客和作者自己的 mine 照常看得到，详情也不受影响。
func TestCommunityBlocksFilterCatalogs(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	mux := blockMux(&Service{store: db})
	author := complete(t, db, Identity{"email", "block-filter-author@example.test"})
	friend := complete(t, db, Identity{"email", "block-filter-friend@example.test"})
	blocker := complete(t, db, Identity{"anonymous", "block-filter-device"})
	bystander := complete(t, db, Identity{"email", "block-filter-bystander@example.test"})
	const (
		candidateAuthor = "b2334455-1234-4234-8234-0000000000a1"
		candidateFriend = "b2334455-1234-4234-8234-0000000000a2"
		pluginAuthor    = "b2334455-1234-4234-8234-0000000000a3"
		pluginFriend    = "b2334455-1234-4234-8234-0000000000a4"
	)
	insertCandidateSkin(t, db, candidateAuthor, author.User.ID, "作者候选窗")
	insertCandidateSkin(t, db, candidateFriend, friend.User.ID, "朋友候选窗")
	for _, q := range []string{
		`INSERT INTO community_skins(id,owner_id,name,design) VALUES('block-skin-author',$1,'作者键盘','` + communityFixture + `'),('block-skin-friend',$2,'朋友键盘','` + communityFixture + `')`,
		`INSERT INTO community_plugins(id,owner_id,kind,plugin_id,name,version,license,manifest,archive,request_sha256) VALUES('` + pluginAuthor + `',$1,'sound','block.author','作者插件','1.0','MIT','合成清单','合成归档',repeat('a',64)),('` + pluginFriend + `',$2,'sound','block.friend','朋友插件','1.0','MIT','合成清单','合成归档',repeat('b',64))`,
		`INSERT INTO community_resources(id,owner_id,kind,name,content) VALUES
 ('block-dict-author',$1,'dictionary','作者词库','{"entries":[{"kind":"pinyin","code":"fangdou","word":"防抖","weight":10}]}'),
 ('block-dict-friend',$2,'dictionary','朋友词库','{"entries":[{"kind":"pinyin","code":"shuihe","word":"水合","weight":9}]}'),
 ('block-reply-author',$1,'reply','作者模板','{"prompt":"今天没法加班"}'),
 ('block-reply-friend',$2,'reply','朋友模板','{"prompt":"改天再约"}'),
 ('block-phrase-author',$1,'phrase','作者短语','{"phrases":[{"text":"你好"}]}'),
 ('block-phrase-friend',$2,'phrase','朋友短语','{"phrases":[{"text":"再见"}]}')`,
	} {
		if _, err := db.pool.Exec(ctx, q, author.User.ID, friend.User.ID); err != nil {
			t.Fatal(err)
		}
	}
	// 屏蔽者先收藏两位作者的作品，验证 saved 范围同样过滤。
	for _, path := range []string{"/v1/community/skins/block-skin-author", "/v1/community/skins/block-skin-friend", "/v1/community/candidate-skins/" + candidateAuthor, "/v1/community/candidate-skins/" + candidateFriend, "/v1/community/plugins/" + pluginAuthor, "/v1/community/plugins/" + pluginFriend, "/v1/community/resources/block-dict-author", "/v1/community/resources/block-dict-friend"} {
		apiRequest(t, mux, "PUT", path+"/save", `{"saved":true}`, blocker.AccessToken, 200)
	}
	catalogs := []struct {
		name, path, field, author, friend string
	}{
		{"skins", "/v1/community/skins", "skins", "block-skin-author", "block-skin-friend"},
		{"candidate-skins", "/v1/community/candidate-skins", "skins", candidateAuthor, candidateFriend},
		{"plugins", "/v1/community/plugins", "plugins", pluginAuthor, pluginFriend},
		{"dictionaries", "/v1/community/resources?kind=dictionary", "items", "block-dict-author", "block-dict-friend"},
		{"replies", "/v1/community/resources?kind=reply", "items", "block-reply-author", "block-reply-friend"},
		{"phrases", "/v1/community/resources?kind=phrase", "items", "block-phrase-author", "block-phrase-friend"},
	}
	listed := func(t *testing.T, path, field, scope, token string) []string {
		t.Helper()
		if scope != "" {
			if strings.Contains(path, "?") {
				path += "&scope=" + scope
			} else {
				path += "?scope=" + scope
			}
		}
		w := apiRequest(t, mux, "GET", path, "", token, 200)
		var body map[string]json.RawMessage
		var page []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || json.Unmarshal(body[field], &page) != nil {
			t.Fatal(err, w.Body.String())
		}
		var ids []string
		for _, item := range page {
			ids = append(ids, item.ID)
		}
		return ids
	}
	check := func(t *testing.T, blocked bool) {
		t.Helper()
		// 每轮检查发出几十个请求，清掉按地址的额度，免得撞上每分钟 120 次的上限。
		if _, err := db.pool.Exec(ctx, `TRUNCATE auth_rates`); err != nil {
			t.Fatal(err)
		}
		for _, c := range catalogs {
			t.Run(c.name, func(t *testing.T) {
				if ids := listed(t, c.path, c.field, "", blocker.AccessToken); slices.Contains(ids, c.author) == blocked || !slices.Contains(ids, c.friend) {
					t.Fatal("屏蔽者的公开目录：", blocked, ids)
				}
				for _, token := range []string{"", bystander.AccessToken} {
					if ids := listed(t, c.path, c.field, "", token); !slices.Contains(ids, c.author) || !slices.Contains(ids, c.friend) {
						t.Fatal("别人的公开目录受到了屏蔽影响：", ids)
					}
				}
				if ids := listed(t, c.path, c.field, "mine", author.AccessToken); !slices.Contains(ids, c.author) {
					t.Fatal("作者自己的 mine 范围受到了屏蔽影响：", ids)
				}
			})
		}
		for _, c := range catalogs[:4] {
			if ids := listed(t, c.path, c.field, "saved", blocker.AccessToken); slices.Contains(ids, c.author) == blocked || !slices.Contains(ids, c.friend) {
				t.Fatal("屏蔽者的收藏范围：", c.name, blocked, ids)
			}
		}
	}
	check(t, false)
	block := blockCreate(t, mux, blocker.AccessToken, "phrases", "block-phrase-author", 201)
	check(t, true)
	// 屏蔽者自己发布的作品在 mine 范围不受影响（屏蔽只看作者，不看浏览者自己的作品）。
	if _, err := db.pool.Exec(ctx, `INSERT INTO community_skins(id,owner_id,name,design) VALUES('block-skin-blocker',$1,'屏蔽者键盘','`+communityFixture+`')`, blocker.User.ID); err != nil {
		t.Fatal(err)
	}
	if ids := listed(t, "/v1/community/skins", "skins", "mine", blocker.AccessToken); !slices.Equal(ids, []string{"block-skin-blocker"}) {
		t.Fatal(ids)
	}
	// 详情、下载和收藏不受屏蔽影响。
	apiRequest(t, mux, "GET", "/v1/community/skins/block-skin-author", "", blocker.AccessToken, 200)
	apiRequest(t, mux, "GET", "/v1/community/candidate-skins/"+candidateAuthor, "", blocker.AccessToken, 200)
	apiRequest(t, mux, "GET", "/v1/community/plugins/"+pluginAuthor, "", blocker.AccessToken, 200)
	apiRequest(t, mux, "GET", "/v1/community/resources/block-reply-author", "", blocker.AccessToken, 200)
	apiRequest(t, mux, "DELETE", "/v1/community/blocks/"+block.ID, "", blocker.AccessToken, 200)
	check(t, false)
}

// 任意一方注销账号，屏蔽记录随外键级联删除。
func TestCommunityBlocksFollowAccountDeletion(t *testing.T) {
	db, _, owner, reader, _ := moderationFixture(t)
	ctx := context.Background()
	mux := blockMux(&Service{store: db})
	third := complete(t, db, Identity{"email", "block-cascade-third@example.test"})
	if _, err := db.pool.Exec(ctx, `INSERT INTO community_skins(id,owner_id,name,design) VALUES('block-skin-third',$1,'第三方','`+communityFixture+`')`, third.User.ID); err != nil {
		t.Fatal(err)
	}
	blockCreate(t, mux, reader.AccessToken, "skins", "skin-a", 201)
	blockCreate(t, mux, reader.AccessToken, "skins", "block-skin-third", 201)
	blockCreate(t, mux, third.AccessToken, "skins", "skin-b", 201)
	count := func() int {
		var n int
		if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM community_blocks`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// 被屏蔽的作者注销：所有屏蔽他的记录消失。
	apiRequest(t, mux, "DELETE", "/v1/users/me", "", owner.AccessToken, 204)
	if n := count(); n != 1 {
		t.Fatal("被屏蔽者注销后剩余屏蔽：", n)
	}
	if got := blockList(t, mux, reader.AccessToken); len(got) != 1 {
		t.Fatal(got)
	}
	// 屏蔽者注销：他的屏蔽记录消失。
	apiRequest(t, mux, "DELETE", "/v1/users/me", "", reader.AccessToken, 204)
	if n := count(); n != 0 {
		t.Fatal("屏蔽者注销后剩余屏蔽：", n)
	}
}

// 缺少屏蔽表的旧库在 Ready 时要求迁移，迁移可以重复执行并补齐表和索引。
func TestCommunityBlocksMigration(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `DROP TABLE community_blocks`); err != nil {
		t.Fatal(err)
	}
	if err := db.Ready(ctx); err == nil {
		t.Fatal("缺少 community_blocks 时仍然就绪")
	}
	for range 2 {
		if err := db.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	var indexed bool
	if err := db.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid WHERE i.indrelid='community_blocks'::regclass AND c.relname='community_blocks_blocked')`).Scan(&indexed); err != nil || !indexed {
		t.Fatal("缺少 blocked_id 索引", err)
	}
}

// 屏蔽接口的每一条数据库语句失败时都返回 5xx，不报告成功，也不留下半截写入。
func TestCommunityBlocksAnswerEveryDatabaseFailure(t *testing.T) {
	db := testStore(t)
	for _, tc := range []struct {
		name, method, path, body string
		status                   int
	}{
		{"create", "POST", "/v1/community/blocks", `{"kind":"skins","item_id":"block-fail-skin"}`, 201},
		{"list", "GET", "/v1/community/blocks", "", 200},
		{"delete", "DELETE", "/v1/community/blocks/00000000-0000-4000-8000-000000000002", "", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace := &statementCancellation{}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = trace
			pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			mux := blockMux(&Service{store: &Store{pool: pool}})
			seed := func() string {
				if _, err := db.pool.Exec(t.Context(), `TRUNCATE auth_users,auth_rates CASCADE`); err != nil {
					t.Fatal(err)
				}
				author := complete(t, db, Identity{"email", "block-fail-author@example.test"})
				user := complete(t, db, Identity{"email", "block-fail-user@example.test"})
				if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_skins(id,owner_id,name,design) VALUES('block-fail-skin',$1,'失败','`+communityFixture+`')`, author.User.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_blocks(id,user_id,blocked_id) VALUES('00000000-0000-4000-8000-000000000002',$1,$2)`, user.User.ID, author.User.ID); err != nil {
					t.Fatal(err)
				}
				if tc.name == "create" {
					if _, err := db.pool.Exec(t.Context(), `DELETE FROM community_blocks`); err != nil {
						t.Fatal(err)
					}
				}
				return user.AccessToken
			}
			call := func(token string) *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, jsonRequest(tc.method, tc.path, tc.body, token))
				return w
			}
			token := seed()
			trace.mu.Lock()
			trace.statements = nil
			trace.mu.Unlock()
			baseline := call(token)
			if baseline.Code != tc.status {
				t.Fatal("baseline", baseline.Code, baseline.Body.String())
			}
			trace.mu.Lock()
			statements := len(trace.statements)
			trace.mu.Unlock()
			for i := 1; i <= statements; i++ {
				token := seed()
				trace.mu.Lock()
				trace.at, trace.seen, trace.statements = i, 0, nil
				trace.mu.Unlock()
				w := call(token)
				trace.mu.Lock()
				trace.at = 0
				trace.mu.Unlock()
				// 不影响结果的语句（例如会话活跃时间）失败时可以照常成功；其余必须报 5xx 且不留下写入。
				if w.Code == baseline.Code {
					continue
				}
				if w.Code < 500 {
					t.Fatalf("statement %d: HTTP %d %s", i, w.Code, w.Body.String())
				}
				var n int
				if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM community_blocks`).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if want := map[string]int{"create": 0, "list": 1, "delete": 1}[tc.name]; n != want {
					t.Fatalf("statement %d 失败后屏蔽记录数 %d，应为 %d", i, n, want)
				}
			}
		})
	}
}

func TestCanonicalUUID(t *testing.T) {
	for id, want := range map[string]bool{
		"00000000-0000-4000-8000-000000000000":  true,
		"ABCDEF01-2345-6789-abcd-ef0123456789":  true,
		"":                                      false,
		"00000000-0000-4000-8000-0000000000000": false,
		"00000000-0000-4000-8000_000000000000":  false,
		"0000000g-0000-4000-8000-000000000000":  false,
		"00000000000000000000000000000000":      false,
	} {
		if canonicalUUID(id) != want {
			t.Error(id, want)
		}
	}
}
