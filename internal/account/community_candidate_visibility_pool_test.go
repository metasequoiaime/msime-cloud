package account

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCommunityCandidateVisibilityWorksWithOnePoolConnection(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "candidate-visibility-pool@example.test"})
	id := "01234567-1234-4234-8234-012345678902"
	insertCandidateSkin(t, db, id, owner.User.ID, "单连接测试")

	cfg := db.pool.Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	mux := http.NewServeMux()
	Mount(mux, a)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	r := httptest.NewRequest(http.MethodPatch, "/v1/community/candidate-skins/"+id, strings.NewReader(`{"category":"tech"}`)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+owner.AccessToken)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("单连接池上的分类更新失败：%d %s", w.Code, w.Body.String())
	}
}
