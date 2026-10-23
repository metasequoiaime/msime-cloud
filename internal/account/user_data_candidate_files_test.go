package account

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestUserDataSummaryIncludesCandidateFiles(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "summary-files@example.test"})
	other := complete(t, db, Identity{"email", "summary-files-other@example.test"})
	ctx := t.Context()
	mux := http.NewServeMux()
	Mount(mux, &Service{store: db})
	readSummary := func() (int64, UserDataSection) {
		t.Helper()
		w := userDataRequest(mux, "GET", "/v1/users/me/data", owner.AccessToken, "")
		var response struct {
			Bytes    int64             `json:"bytes"`
			Sections []UserDataSection `json:"sections"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil {
			t.Fatalf("读取汇总失败：%d %s", w.Code, w.Body.String())
		}
		for _, section := range response.Sections {
			if section.ID == "community" {
				return response.Bytes, section
			}
		}
		t.Fatal("汇总缺少社区分区")
		return 0, UserDataSection{}
	}
	for _, skin := range []struct{ id, user, visibility string }{
		{"00000000-0000-4000-8000-000000000001", owner.User.ID, "public"},
		{"00000000-0000-4000-8000-000000000002", owner.User.ID, "private"},
		{"00000000-0000-4000-8000-000000000003", owner.User.ID, "private"},
		{"00000000-0000-4000-8000-000000000004", other.User.ID, "public"},
	} {
		if _, err := db.pool.Exec(ctx, `INSERT INTO community_candidate_skins(id,owner_id,package_id,name,version,license_assets,manifest,preview_path,request_sha256,visibility)
VALUES($1,$2,'summary-files','汇总资源','1','MIT','x','preview.png',repeat('a',64),$3)`, skin.id, skin.user, skin.visibility); err != nil {
			t.Fatal(err)
		}
	}
	beforeTotal, before := readSummary()
	image := feedbackPNG(t, 40, 80)
	for _, file := range []struct{ skin, path string }{
		{"00000000-0000-4000-8000-000000000001", "preview.png"},
		{"00000000-0000-4000-8000-000000000001", "background.png"},
		{"00000000-0000-4000-8000-000000000002", "preview.png"},
		{"00000000-0000-4000-8000-000000000004", "preview.png"},
	} {
		if _, err := db.pool.Exec(ctx, `INSERT INTO community_candidate_skin_files(skin_id,path,bytes) VALUES($1,$2,$3)`, file.skin, file.path, image); err != nil {
			t.Fatal(err)
		}
	}
	var fileBytes int64
	if err := db.pool.QueryRow(ctx, `SELECT sum(pg_column_size(f.*)) FROM community_candidate_skin_files f
JOIN community_candidate_skins s ON s.id=f.skin_id WHERE s.owner_id=$1`, owner.User.ID).Scan(&fileBytes); err != nil {
		t.Fatal(err)
	}
	afterTotal, after := readSummary()
	if fileBytes <= 0 || after.Bytes-before.Bytes != fileBytes || afterTotal-beforeTotal != fileBytes {
		t.Fatalf("图片未按所属账号计入汇总：分区增量=%d 总量增量=%d 图片行大小=%d", after.Bytes-before.Bytes, afterTotal-beforeTotal, fileBytes)
	}
	if before.Items != 3 || after.Items != 3 {
		t.Fatalf("图片数量不应改变作品数量：之前=%d 之后=%d", before.Items, after.Items)
	}
	if _, err := db.pool.Exec(ctx, `DELETE FROM community_candidate_skin_files WHERE skin_id IN (
SELECT id FROM community_candidate_skins WHERE owner_id=$1)`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	finalTotal, final := readSummary()
	if finalTotal != beforeTotal || final != before {
		t.Fatalf("删除图片后统计未恢复：之前=%+v 最后=%+v", before, final)
	}
}
