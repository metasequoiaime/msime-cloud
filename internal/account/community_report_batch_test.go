package account

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type communityReportTrace struct {
	mu            sync.Mutex
	queries       int
	notifications int
}

func (t *communityReportTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	t.mu.Lock()
	defer t.mu.Unlock()
	if strings.Contains(data.SQL, "community_reports") || strings.Contains(data.SQL, "community_skins") {
		t.queries++
	}
	if strings.Contains(data.SQL, "admin_notifications") {
		t.notifications++
	}
	return ctx
}

func (*communityReportTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestCommunityReportVisibilityAndInsertUseOneDatabaseStatement(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "community-report-batch@example.test"})
	if _, err := db.pool.Exec(t.Context(), `DELETE FROM admin_notifications WHERE target_page='community' AND target_id='skins/community-report-batch-skin'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_skins(id,owner_id,name,design) VALUES('community-report-batch-skin',$1,'举报测试','{}')`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	cfg := db.pool.Config()
	trace := &communityReportTrace{}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/community/reports", Route(a, "POST /v1/community/reports", (*Service).CommunityReport))
	apiRequest(t, mux, "POST", "/v1/community/reports", `{"kind":"skins","item_id":"community-report-batch-skin","reason":"重复内容"}`, owner.AccessToken, 201)
	trace.mu.Lock()
	queries, notifications := trace.queries, trace.notifications
	trace.mu.Unlock()
	if queries != 1 || notifications != 1 {
		t.Fatalf("举报可见性和写入使用 %d 条语句、通知使用 %d 条语句，期望各 1 条", queries, notifications)
	}
	var title string
	if err := db.pool.QueryRow(t.Context(), `SELECT title FROM admin_notifications WHERE kind=$1 AND target_page='community' AND target_id=$2`, NotifyReport, "skins/community-report-batch-skin").Scan(&title); err != nil || title != "皮肤「举报测试」被举报：重复内容" {
		t.Fatalf("举报通知标题 = %q，错误 = %v", title, err)
	}
}

// 举报通知沿用统一标题规范化，连续空白应折叠，超长历史名称应截断。
func TestCommunityReportNormalizesNotificationTitle(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "report-normalize@example.test"})
	mux := http.NewServeMux()
	a := &Service{store: db}
	mux.HandleFunc("POST /v1/community/reports", Route(a, "POST /v1/community/reports", (*Service).CommunityReport))
	for _, tc := range []struct {
		id, name, reason, want string
	}{
		{"spaces", "合成  名称", "重复  内容", "皮肤「合成 名称」被举报：重复 内容"},
		{"unicode", "合成\u2003名称", "重复\u00a0内容", "皮肤「合成 名称」被举报：重复 内容"},
		{"long", strings.Repeat("旧", 310), "重复内容", "皮肤「" + strings.Repeat("旧", 296) + "…"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			id := "report-normalize-" + tc.id
			if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_skins(id,owner_id,name,design) VALUES($1,$2,$3,'{}')`, id, owner.User.ID, tc.name); err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(map[string]string{"kind": "skins", "item_id": id, "reason": tc.reason})
			if err != nil {
				t.Fatal(err)
			}
			since := lastNotificationID(t, db)
			apiRequest(t, mux, "POST", "/v1/community/reports", string(body), owner.AccessToken, 201)
			var title string
			if err := db.pool.QueryRow(t.Context(), `SELECT title FROM admin_notifications WHERE kind=$1 AND target_id=$2 AND id>$3`, NotifyReport, "skins/"+id, since).Scan(&title); err != nil || title != tc.want {
				t.Fatalf("通知标题 = %q，期望 %q，错误 = %v", title, tc.want, err)
			}
		})
	}
}
