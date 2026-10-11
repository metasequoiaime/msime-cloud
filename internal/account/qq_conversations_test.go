package account

import (
	"context"
	"testing"
)

func TestQQConversationWindowAndIsolation(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	ctx := context.Background()
	for _, q := range []string{"一", "二", "三"} {
		if err := a.RecordQQTurn(ctx, "G/M1", q, "答"+q); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.RecordQQTurn(ctx, "G/M2", "别人的", "答"); err != nil {
		t.Fatal(err)
	}
	// 超过 30 分钟的不读。
	if _, err := db.pool.Exec(ctx, `INSERT INTO qq_conversations(id,subject,question,answer,created_at) VALUES('old',$1,'很早以前','答',now()-interval '31 minutes')`, hash("G/M1")); err != nil {
		t.Fatal(err)
	}
	turns, err := a.QQConversation(ctx, "G/M1", 2)
	if err != nil || len(turns) != 2 || turns[0].Question != "二" || turns[1].Question != "三" || turns[1].Answer != "答三" {
		t.Fatal(turns, err)
	}
	if turns, _ := a.QQConversation(ctx, "G/M3", 2); len(turns) != 0 {
		t.Fatal("other members must not see this conversation", turns)
	}
	// 只存摘要，不存原始的群和成员标识。
	var raw int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM qq_conversations WHERE subject LIKE 'G/%'`).Scan(&raw); err != nil || raw != 0 {
		t.Fatal(raw, err)
	}
	db.Prune(ctx)
	var left int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM qq_conversations WHERE id='old'`).Scan(&left); err != nil || left != 0 {
		t.Fatal("prune must delete expired turns", left, err)
	}
	// 数据库不可用时报错。
	db.pool.Close()
	if _, err := a.QQConversation(ctx, "G/M1", 2); err == nil {
		t.Fatal("closed database accepted")
	}
	if err := a.RecordQQTurn(ctx, "G/M1", "q", "a"); err == nil {
		t.Fatal("closed database accepted")
	}
}
