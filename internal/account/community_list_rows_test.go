package account

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type communityRowsItem struct {
	ID int `json:"id"`
}

func scanCommunityRowsItem(row pgx.CollectableRow) (communityRowsItem, error) {
	var item communityRowsItem
	err := row.Scan(&item.ID)
	return item, err
}

func TestCommunityRowsPageResponse(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"email", "community-rows@example.test"})
	cfg := db.pool.Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	for _, field := range []string{"skins", "plugins", "items"} {
		for _, count := range []int{0, 1, 20, 21} {
			t.Run(fmt.Sprintf("%s/%d", field, count), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				rows, err := pool.Query(ctx, "SELECT n FROM generate_series(1,$1::int) n", count)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				r := httptest.NewRequest(http.MethodGet, "/v1/community/skins?scope=saved", nil).WithContext(ctx)
				w := httptest.NewRecorder()
				writeCommunityRows(a, w, r, "saved", user.User.ID, field, rows, scanCommunityRowsItem)
				var body map[string]json.RawMessage
				if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body) != 2 {
					t.Fatalf("分页响应错误：%d %s", w.Code, w.Body.String())
				}
				var items []communityRowsItem
				var more bool
				if json.Unmarshal(body[field], &items) != nil || items == nil || len(items) != min(count, 20) || json.Unmarshal(body["has_more"], &more) != nil || more != (count > 20) {
					t.Fatalf("分页条目或边界错误：%s", w.Body.String())
				}
				for i, item := range items {
					if item.ID != i+1 {
						t.Fatalf("条目顺序错误：%v", items)
					}
				}
			})
		}
	}
}

func TestCommunityRowsRejectReadErrors(t *testing.T) {
	db := testStore(t)
	cfg := db.pool.Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	for _, tc := range []struct{ name, query string }{
		{"扫描失败", "SELECT 'synthetic-invalid-number'::text"},
		{"部分行后读取失败", "SELECT 10/(3-n) FROM generate_series(1,3) n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			rows, err := pool.Query(ctx, tc.query)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			r := httptest.NewRequest(http.MethodGet, "/v1/community/skins?scope=mine", nil).WithContext(ctx)
			w := httptest.NewRecorder()
			writeCommunityRows(a, w, r, "mine", "missing-user", "skins", rows, scanCommunityRowsItem)
			if want := "{\"error\":{\"code\":\"auth_unavailable\",\"message\":\"auth_unavailable\"}}\n"; w.Code != http.StatusServiceUnavailable || w.Body.String() != want {
				t.Fatalf("读取失败不应写出部分列表或先返回账号错误：%d %s", w.Code, w.Body.String())
			}
			var alive int
			if err := pool.QueryRow(ctx, "SELECT 1").Scan(&alive); err != nil || alive != 1 {
				t.Fatalf("读取失败后未释放唯一连接：%d %v", alive, err)
			}
		})
	}
}

func TestCommunityRowsEmptyScopeRechecksUser(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	for _, scope := range []string{"", "mine", "saved"} {
		t.Run(scope, func(t *testing.T) {
			rows, err := db.pool.Query(t.Context(), "SELECT 1 WHERE false")
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			r := httptest.NewRequest(http.MethodGet, "/v1/community/skins", nil)
			w := httptest.NewRecorder()
			writeCommunityRows(a, w, r, scope, "missing-user", "skins", rows, scanCommunityRowsItem)
			status, want := http.StatusOK, "{\"has_more\":false,\"skins\":[]}\n"
			if scope != "" {
				status, want = http.StatusUnauthorized, "{\"error\":{\"code\":\"invalid_credentials\",\"message\":\"invalid_credentials\"}}\n"
			}
			if w.Code != status || w.Body.String() != want {
				t.Fatalf("空列表账号复查发生变化：%d %s", w.Code, w.Body.String())
			}
		})
	}
}
