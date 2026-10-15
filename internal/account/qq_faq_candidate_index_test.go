package account

import (
	"strings"
	"testing"
)

func TestQQFAQCandidatesHaveWritingClaimIndex(t *testing.T) {
	db := testStore(t)
	var indexDef string
	err := db.pool.QueryRow(t.Context(), `SELECT indexdef FROM pg_indexes WHERE schemaname=current_schema() AND indexname='qq_faq_candidates_writing_claimed'`).Scan(&indexDef)
	if err != nil || !strings.Contains(indexDef, "(claimed_at, created_at, id)") || !strings.Contains(indexDef, "state = 'writing'::text") {
		t.Fatalf("FAQ 候选超时认领索引缺失或定义错误：%q，错误=%v", indexDef, err)
	}
}
