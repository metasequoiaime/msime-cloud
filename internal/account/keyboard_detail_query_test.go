package account

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestKeyboardDetailReadsCompleteDesignOnce(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "detail-query-owner@example.test"})
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_skins(id,owner_id,name,design) VALUES('detail-query',$1,'example','{"background":12,"photo":"cGhvdG8="}')`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	cfg := db.pool.Config()
	trace := &communityPaginationTrace{table: "community_skins", queries: make(chan communityPageQuery, 8)}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	for _, fields := range []string{"", "?fields=saved"} {
		t.Run("detail"+fields, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/v1/community/skins/detail-query"+fields, nil)
			r.SetPathValue("id", "detail-query")
			w := httptest.NewRecorder()
			a.communityDetail(w, r)
			var item CommunitySkin
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &item) != nil {
				t.Fatal(w.Code, w.Body.String())
			}
			var design map[string]any
			if err := json.Unmarshal(item.Design, &design); err != nil || design["photo"] != "cGhvdG8=" || design["background"] != float64(12) {
				t.Fatal("详情未返回完整设计：", err, string(item.Design))
			}
			queries := len(trace.queries)
			for range queries {
				<-trace.queries
			}
			if queries != 1 {
				t.Errorf("详情读取皮肤 %d 次，期望 1 次", queries)
			}
		})
	}
	r := httptest.NewRequest("GET", "/v1/community/skins", nil)
	w := httptest.NewRecorder()
	a.communityList(w, r)
	var body struct {
		Skins []CommunitySkin `json:"skins"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body.Skins) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	var design map[string]any
	if err := json.Unmarshal(body.Skins[0].Design, &design); err != nil {
		t.Fatal(err)
	}
	if _, ok := design["photo"]; ok {
		t.Fatal("目录不应返回完整图片")
	}
}
