package account

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/metasequoiaime/MSIME-Backend/internal/httpjson"

	"github.com/jackc/pgx/v5"
)

// 编辑凭据包含作品内容和审核状态；互动计数不参与，避免下载或评分让表单失效。
func contentEditToken(section, alias string) string {
	fields := alias + `.id,` + alias + `.owner_id,` + alias + `.created_at,` + alias + `.name,` + alias + `.description,` + alias + `.moderation,` + alias + `.moderated_at`
	switch section {
	case "skins":
		fields += "," + alias + `.design,` + alias + `.category`
	case "plugins":
		fields += "," + alias + `.request_sha256,` + alias + `.sha256`
	case "candidate-skins":
		fields += "," + alias + `.request_sha256,` + alias + `.updated_at,` + alias + `.category,` + alias + `.visibility`
	}
	return `md5(jsonb_build_array(` + fields + `)::text)`
}

type contentEdit struct {
	OwnerID     string            `json:"owner_id"`
	EditToken   string            `json:"edit_token"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Moderation  string            `json:"moderation"`
	Category    string            `json:"category"`
	Design      json.RawMessage   `json:"design"`
	Archive     []byte            `json:"archive"`
	Manifest    string            `json:"manifest"`
	Files       map[string][]byte `json:"files"`
	Visibility  string            `json:"visibility"`
}

// 新增和编辑都由统一的操作分发器提交，作品写入和审计在同一事务内完成。
func saveCommunityContent(section string, create bool) adminActionFunc {
	// 分区和表名固定，预先构建查询；作品版本和审核状态仍由数据库逐请求读取或更新。
	table := moderationSections[section].table
	lockQuery := `SELECT owner_id,` + contentEditToken(section, "s") + ` FROM ` + table + ` s WHERE id=$1 FOR UPDATE`
	moderationQuery := `UPDATE ` + table + ` SET moderation=$2,previous_moderation=CASE WHEN $2='removed' THEN CASE WHEN $4 THEN 'approved' WHEN moderation='removed' THEN COALESCE(previous_moderation,'approved') ELSE moderation END END,moderation_reason=CASE WHEN $2='removed' THEN COALESCE(moderation_reason,'管理员保存为下架') END,moderated_by=$3,moderated_at=now() WHERE id=$1`
	return func(a *Service, ctx context.Context, tx pgx.Tx, v actionRequest) (actionResult, error) {
		if err := requireActionID(v); err != nil {
			return actionResult{}, err
		}
		if len(v.IDs) != 0 || (v.Section != "" && v.Section != section) || (create && (len(v.ID) != 36 || !validCommunityID(v.ID))) {
			return actionResult{}, actionFail(400, "invalid_value")
		}
		var input contentEdit
		if httpjson.DecodeBytes(v.Value, &input) != nil {
			return actionResult{}, actionFail(400, "invalid_value")
		}
		input.Name, input.Description = strings.TrimSpace(input.Name), strings.TrimSpace(input.Description)
		if !validCommunityTextMetadata(input.Name, input.Description) || !validUserID(input.OwnerID) {
			return actionResult{}, actionFail(400, "invalid_value")
		}
		if input.Moderation != "pending" && input.Moderation != "approved" && input.Moderation != "removed" {
			return actionResult{}, actionFail(400, "invalid_value")
		}
		if section != "plugins" && !validCandidateSkinCategory(input.Category) {
			return actionResult{}, actionFail(400, "invalid_category")
		}
		// 先锁作者再锁作品，与封禁及客户端写入保持同一锁顺序。
		owner, err := lockUserForBan(ctx, tx, input.OwnerID)
		if err != nil {
			return actionResult{}, err
		}
		if owner.Banned {
			return actionResult{}, actionFail(403, "account_banned")
		}
		if !create {
			var owner, token string
			err = tx.QueryRow(ctx, lockQuery, v.ID).Scan(&owner, &token)
			if errors.Is(err, pgx.ErrNoRows) {
				return actionResult{}, actionFail(404, "not_found")
			}
			if err != nil {
				return actionResult{}, err
			}
			if owner != input.OwnerID || token != input.EditToken {
				return actionResult{}, actionFail(409, "content_changed")
			}
		}
		switch section {
		case "skins":
			err = saveAdminSkin(ctx, tx, v.ID, input, create)
		case "plugins":
			err = saveAdminPlugin(ctx, tx, v.ID, input, create)
		case "candidate-skins":
			err = saveAdminCandidate(ctx, tx, v.ID, input, create)
		}
		if err != nil {
			return actionResult{}, err
		}
		// 包体和设计数据不进入审计，只记操作对象与发布状态。
		_, err = tx.Exec(ctx, moderationQuery, v.ID, input.Moderation, adminActor(ctx), create)
		return actionResult{Affected: 1, Detail: map[string]any{"section": section, "name": input.Name, "moderation": input.Moderation}, Extra: map[string]any{"id": v.ID}}, err
	}
}

func saveAdminSkin(ctx context.Context, tx pgx.Tx, id string, input contentEdit, create bool) error {
	if input.Archive != nil || input.Manifest != "" || input.Files != nil || input.Visibility != "" {
		return actionFail(400, "invalid_value")
	}
	design, err := parseCommunityDesign(input.Design)
	if err != nil {
		return actionFail(400, "invalid_skin_design")
	}
	raw, err := json.Marshal(design)
	if err != nil {
		return err
	}
	photoHash, err := communityDesignPhotoHash(design)
	if err != nil {
		return err
	}
	// 来源与用户发布一样按照片判定；修改只会在新照片匹配到插画时把来源抬成 ai，其余情况保留原值，需要改判时用 set_skin_origin。
	if create {
		tag, err := tx.Exec(ctx, `INSERT INTO community_skins(id,owner_id,name,description,design,category,origin) VALUES($1,$2,$3,$4,$5,$6,`+skinPublishOriginSQL("false", "$7")+`) ON CONFLICT(id) DO NOTHING`, id, input.OwnerID, input.Name, input.Description, raw, input.Category, photoHash)
		if err == nil && tag.RowsAffected() == 0 {
			return actionFail(409, "skin_id_conflict")
		}
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE community_skins SET name=$2,description=$3,design=$4,category=$5,origin=CASE WHEN `+skinArtworkMatchSQL("$6")+` THEN 'ai' ELSE origin END WHERE id=$1`, id, input.Name, input.Description, raw, input.Category, photoHash)
	return err
}

func saveAdminPlugin(ctx context.Context, tx pgx.Tx, id string, input contentEdit, create bool) error {
	if len(input.Design) != 0 || input.Manifest != "" || input.Files != nil || input.Category != "" || input.Visibility != "" {
		return actionFail(400, "invalid_value")
	}
	archive := input.Archive
	if archive == nil && !create {
		if err := tx.QueryRow(ctx, `SELECT archive FROM community_plugins WHERE id=$1`, id).Scan(&archive); err != nil {
			return err
		}
	}
	release, ok := acquirePluginSlot(ctx, pluginUploadSlots)
	if !ok {
		return actionFail(503, "plugin_busy")
	}
	defer release()
	pack, code := validPluginArchive(archive)
	if code != "" {
		return actionFail(400, code)
	}
	digest := pluginRequestDigest(input.Name, input.Description, pack.Kind, pack.ID, pack.Version, archive)
	args := []any{id, input.OwnerID, pack.Kind, pack.ID, input.Name, input.Description, pack.Version, pack.License, pack.Manifest, archive, digest, pack.Permissions}
	if create {
		tag, err := tx.Exec(ctx, `INSERT INTO community_plugins(id,owner_id,kind,plugin_id,name,description,version,license,manifest,archive,request_sha256,permissions) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT(id) DO NOTHING`, args...)
		if err == nil && tag.RowsAffected() == 0 {
			return actionFail(409, "plugin_id_conflict")
		}
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE community_plugins SET kind=$3,plugin_id=$4,name=$5,description=$6,version=$7,license=$8,manifest=$9,archive=$10,request_sha256=$11,permissions=$12 WHERE id=$1 AND owner_id=$2`, args...)
	return err
}

func saveAdminCandidate(ctx context.Context, tx pgx.Tx, id string, input contentEdit, create bool) error {
	if len(input.Design) != 0 || input.Archive != nil || (input.Visibility != "public" && input.Visibility != "private") {
		return actionFail(400, "invalid_value")
	}
	keepFiles := input.Files == nil && !create
	if keepFiles {
		rows, err := tx.Query(ctx, `SELECT path,bytes FROM community_candidate_skin_files WHERE skin_id=$1`, id)
		if err != nil {
			return err
		}
		input.Files, err = collectCandidateFiles(rows)
		if err != nil {
			return err
		}
	}
	if code := validCandidateResourceFiles(input.Manifest, input.Files, true); code != "" {
		return actionFail(400, code)
	}
	pkg, code := validCandidatePackage(input.Manifest, input.Files, input.Visibility == "public")
	if code != "" {
		return actionFail(400, code)
	}
	var clean map[string][]byte
	if keepFiles {
		// 已存图片经过校验及清理，元数据编辑不再编码，避免 JPEG 反复压缩。
		clean = input.Files
	} else if !withCandidateImageSlot(ctx, func() { clean, code = sanitizeCandidateImages(input.Files, pkg.Preview) }) {
		return actionFail(503, "candidate_skin_busy")
	}
	if code != "" {
		return actionFail(400, code)
	}
	license := candidatePackageLicense(pkg)
	digest := candidateRequestDigest(input.Name, input.Description, input.Manifest, input.Files)
	args := []any{id, input.OwnerID, pkg.ID, input.Name, input.Description, pkg.Version, license.Code, license.Assets, license.Source, []byte(input.Manifest), pkg.Preview, digest, input.Visibility, input.Category}
	if create {
		tag, err := tx.Exec(ctx, `INSERT INTO community_candidate_skins(id,owner_id,package_id,name,description,version,license_code,license_assets,license_source,manifest,preview_path,request_sha256,visibility,category) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) ON CONFLICT(id) DO NOTHING`, args...)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return actionFail(409, "candidate_skin_id_conflict")
		}
	} else {
		_, err := tx.Exec(ctx, `UPDATE community_candidate_skins SET package_id=$3,name=$4,description=$5,version=$6,license_code=$7,license_assets=$8,license_source=$9,manifest=$10,preview_path=$11,request_sha256=$12,visibility=$13,category=$14,updated_at=now() WHERE id=$1 AND owner_id=$2`, args...)
		if err != nil {
			return err
		}
		if keepFiles {
			return nil
		}
		if _, err = tx.Exec(ctx, `DELETE FROM community_candidate_skin_files WHERE skin_id=$1`, id); err != nil {
			return err
		}
	}
	return insertCandidateFiles(ctx, tx, id, clean)
}
