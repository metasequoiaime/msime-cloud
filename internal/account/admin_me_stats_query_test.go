package account

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminMeStatsUsesSinglePassAggregates(t *testing.T) {
	db := testStore(t)
	truncateAdminPersonal(t, db)
	a := &Service{store: db}
	legacy := AdminAccess{Actor: "legacy-token", Role: RoleMaintainer}
	empty := decodeMe(t, meRequest(a, legacy, "GET", "", ""))
	if empty.Stats.DictPRs != 0 || empty.Stats.Issues != 0 || empty.Stats.Community != 0 || empty.Stats.AvgHandleHours != nil {
		t.Fatalf("空表统计错误：%+v", empty.Stats)
	}
	const email = "stats@example.test"
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO auth_users(id) VALUES('me-stats-author');
 INSERT INTO admin_audit(action,target,actor) VALUES
 ('dict_pr_approve','210','google:g1:stats@example.test'),
 ('dict_pr_trim','210','pat:stats@example.test'),
 ('dict_pr_reject','211','google:g2:stats@example.test'),
 ('issue_triage','repo#1','google:g1:stats@example.test'),
 ('issue_comment','repo#1','pat:stats@example.test'),
 ('delete_skin','skin','pat:stats@example.test'),
 ('delete_candidate_skin','candidate','pat:stats@example.test'),
 ('delete_plugin','plugin','pat:stats@example.test'),
 ('delete_dictionary','dictionary','pat:stats@example.test'),
 ('delete_reply','reply','pat:stats@example.test'),
 ('dictXpr_approve','wrong-prefix','pat:stats@example.test'),
 ('issueXcomment','wrong-prefix','pat:stats@example.test'),
 ('dict_pr_approve','other','google:g:xstats@example.test'),
 ('dict_pr_approve','legacy-pr','legacy-token'),
 ('dict_pr_trim','legacy-pr','legacy-token'),
 ('issue_comment','legacy-issue','legacy-token'),
 ('delete_skin','legacy-skin','legacy-token');
 INSERT INTO admin_audit(action,target,actor,created_at) VALUES
 ('dict_pr_approve','old','pat:stats@example.test',now()-interval '40 days');
 INSERT INTO admin_audit(action,target,actor)
 SELECT 'admin_pref_set','weekly_digest',actor FROM generate_series(1,1000) n CROSS JOIN unnest(ARRAY['pat:stats@example.test','legacy-token']) actor;
 INSERT INTO community_skins(id,owner_id,name,design,created_at,moderation,moderated_by,moderated_at)
 SELECT id,'me-stats-author','合成皮肤','{}',now()-hours*interval '1 hour',state,actor,at FROM (VALUES
 ('stats-approved',4,'approved','google:g1:stats@example.test',now()),
 ('stats-removed',2,'removed','pat:stats@example.test',now()),
 ('stats-pending',400,'pending','pat:stats@example.test',now()),
 ('stats-old',1000,'approved','pat:stats@example.test',now()-interval '40 days'),
 ('stats-other',9,'approved','google:g:xstats@example.test',now()),
 ('stats-legacy',8,'approved','legacy-token',now())
 ) v(id,hours,state,actor,at);
 INSERT INTO community_resources(id,owner_id,kind,name,content,created_at,moderation,moderated_by,moderated_at)
 SELECT 'stats-'||kind||'-'||hours,'me-stats-author',kind,'合成资源','{}',now()-hours*interval '1 hour','approved',actor,now() FROM (VALUES
 ('dictionary',6,'pat:stats@example.test'),('reply',8,'google:g2:stats@example.test'),('phrase',10,'pat:stats@example.test'),('reply',12,'legacy-token')
 ) v(kind,hours,actor);
 INSERT INTO community_candidate_skins(id,owner_id,package_id,name,version,license_assets,manifest,preview_path,request_sha256,created_at,moderation,moderated_by,moderated_at)
 VALUES('12345678-1234-4234-8234-123456789abc','me-stats-author','stats-candidate','合成候选皮肤','1','MIT','x','preview.png',repeat('a',64),now()-interval '12 hours','approved','pat:stats@example.test',now());
 INSERT INTO community_plugins(id,owner_id,kind,plugin_id,name,version,license,manifest,archive,request_sha256,created_at,moderation,moderated_by,moderated_at)
 VALUES('22345678-1234-4234-8234-123456789abc','me-stats-author','sound','stats-plugin','合成插件','1','MIT','x','x',repeat('b',64),now()-interval '14 hours','approved','pat:stats@example.test',now());
 ANALYZE admin_audit,community_skins,community_resources,community_candidate_skins,community_plugins;`); err != nil {
		t.Fatal(err)
	}
	trace := &communityPaginationTrace{table: "admin_audit", queries: make(chan communityPageQuery, 2)}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a = &Service{store: &Store{pool: pool}}
	for _, tc := range []struct {
		name             string
		access           AdminAccess
		dict, community  int64
		avg              float64
		matchedAuditRows int
	}{
		{"google", AdminAccess{Email: email, Actor: "google:g1:" + email}, 2, 12, 8, 10},
		{"pat", AdminAccess{Email: email, Actor: "pat:" + email}, 2, 12, 8, 10},
		{"legacy", legacy, 1, 3, 10, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := decodeMe(t, meRequest(a, tc.access, "GET", "", ""))
			if body.Stats.DictPRs != tc.dict || body.Stats.Issues != 1 || body.Stats.Community != tc.community || body.Stats.AvgHandleHours == nil || math.Abs(*body.Stats.AvgHandleHours-tc.avg) > 0.001 {
				t.Fatalf("月度统计语义错误：%+v", body.Stats)
			}
			var query communityPageQuery
			for len(trace.queries) > 0 {
				captured := <-trace.queries
				if strings.HasPrefix(captured.sql, "WITH mine AS (") {
					query = captured
				}
			}
			if query.sql == "" {
				t.Fatal("没有记录到个人中心月度统计查询")
			}
			var raw json.RawMessage
			if err := db.pool.QueryRow(t.Context(), `EXPLAIN (ANALYZE,TIMING OFF,FORMAT JSON) `+query.sql, query.args...).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			type node struct {
				Relation string `json:"Relation Name"`
				CTE      string `json:"CTE Name"`
				Rows     int    `json:"Actual Rows"`
				Loops    int    `json:"Actual Loops"`
				Plans    []node `json:"Plans"`
			}
			var plans []struct{ Plan node }
			if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
				t.Fatal(err, string(raw))
			}
			cteScans := map[string]int{}
			auditRows, seen := 0, false
			var visit func(node)
			visit = func(n node) {
				if n.CTE != "" {
					cteScans[n.CTE] += n.Loops
				}
				if n.Relation == "admin_audit" {
					auditRows += n.Rows * n.Loops
					seen = true
				}
				for _, child := range n.Plans {
					visit(child)
				}
			}
			visit(plans[0].Plan)
			if cteScans["mine"] > 1 || cteScans["moderated"] > 1 {
				t.Errorf("月度统计重复遍历中间结果：%v", cteScans)
			}
			if !seen || auditRows > tc.matchedAuditRows {
				t.Errorf("审计表向聚合传入 %d 行，最多需要 %d 行；找到扫描=%v", auditRows, tc.matchedAuditRows, seen)
			}
		})
	}
}
