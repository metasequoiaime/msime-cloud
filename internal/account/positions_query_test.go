package account

import (
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCandidatePositionsPaginationUsesContextIndex(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO auth_users(id) VALUES('positions-query-owner'),('positions-query-other');
 INSERT INTO user_candidate_positions(user_id,context,code,word,position)
 SELECT 'positions-query-owner','ctx-'||lpad(n::text,5,'0'),'code-'||p,'合成词'||p,p
 FROM generate_series(1,1000) n CROSS JOIN generate_series(1,5) p;
 INSERT INTO user_candidate_positions(user_id,context,code,word,position)
 VALUES('positions-query-other','ctx-01000','other-code','另一个合成词',1);
 ANALYZE user_candidate_positions;`); err != nil {
		t.Fatal(err)
	}
	trace := &communityPaginationTrace{table: "user_candidate_positions", queries: make(chan communityPageQuery, 1)}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	store := &Store{pool: pool}
	position := func(context, suffix string, slot int) CandidatePosition {
		return CandidatePosition{Context: context, Code: "code-" + suffix, Word: "合成词" + suffix, Position: slot}
	}
	for _, tc := range []struct {
		name, user, context string
		offset, limit       int
		want                []CandidatePosition
		more                bool
	}{
		{"筛选首页", "positions-query-owner", "ctx-01000", 0, 2, []CandidatePosition{position("ctx-01000", "1", 1), position("ctx-01000", "2", 2)}, true},
		{"筛选次页", "positions-query-owner", "ctx-01000", 2, 2, []CandidatePosition{position("ctx-01000", "3", 3), position("ctx-01000", "4", 4)}, true},
		{"筛选末页", "positions-query-owner", "ctx-01000", 4, 2, []CandidatePosition{position("ctx-01000", "5", 5)}, false},
		{"筛选超尾页", "positions-query-owner", "ctx-01000", 5, 2, []CandidatePosition{}, false},
		{"不存在上下文", "positions-query-owner", "missing", 0, 2, []CandidatePosition{}, false},
		{"无筛选首页", "positions-query-owner", "", 0, 2, []CandidatePosition{position("ctx-00001", "1", 1), position("ctx-00001", "2", 2)}, true},
		{"无筛选跨上下文", "positions-query-owner", "", 5, 2, []CandidatePosition{position("ctx-00002", "1", 1), position("ctx-00002", "2", 2)}, true},
		{"用户隔离", "positions-query-other", "ctx-01000", 0, 2, []CandidatePosition{{Context: "ctx-01000", Code: "other-code", Word: "另一个合成词", Position: 1}}, false},
		{"不存在用户", "positions-query-missing", "ctx-01000", 0, 2, []CandidatePosition{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, more, err := store.CandidatePositions(t.Context(), tc.user, tc.context, tc.offset, tc.limit)
			if err != nil || more != tc.more || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("固定候选分页结果=%+v，has_more=%t，错误=%v；期望=%+v，has_more=%t", got, more, err, tc.want, tc.more)
			}
			var query communityPageQuery
			select {
			case query = <-trace.queries:
			default:
				t.Fatal("没有记录到固定候选分页查询")
			}
			for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
				t.Run(mode, func(t *testing.T) {
					plan := adminFilteredExplain(t, db, query, mode)
					scanned := 0
					var visit func(adminFilteredPlan)
					visit = func(n adminFilteredPlan) {
						if n.Relation == "user_candidate_positions" {
							scanned += (n.Rows + n.Removed) * n.Loops
						}
						for _, child := range n.Plans {
							visit(child)
						}
					}
					visit(plan)
					// 已有联合索引可直接定位用户和上下文，只需读取偏移与本页额外一行。
					if maximum := tc.offset + tc.limit + 1; scanned > maximum {
						t.Errorf("固定候选分页实际扫描 %d 行，超过 %d 行预算", scanned, maximum)
					}
				})
			}
		})
	}
}
