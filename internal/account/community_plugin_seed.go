package account

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// 社区插件库只存在 PostgreSQL 里。msime-plugins 仓库里维护者制作的精选包经 RenderCommunityPluginSeed 生成的 SQL 一次性导入，由运维审核后自己执行；这里只生成文本，从不连接数据库。每个包走发布接口同一套校验（pluginPublishMetadataCode、validPluginPublishArchive 和每账号配额），所以种子里的包与经接口发布的包受同样的规则约束，规则只有服务端这一份。

// pluginSeedPublisherName 是精选内容的非交互发布主体，与 scripts/community_seed.py、scripts/community_resources_seed.py 使用同一个账号。
const pluginSeedPublisherName = "水杉精选"

var (
	pluginSeedPublisher = uuid5URL("https://msime.app/community/publisher/starter")
	pluginSeedRole      = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
)

// pluginSeedModifiedDate 是 zip 成员固定修改时间 1980-01-01 00:00 的 MS-DOS 日期（时间字段为 0），与客户端 pack() 用的 zip::DateTime::default() 相同。
const pluginSeedModifiedDate = 1<<5 | 1

// pluginSeedPack 是一行待写入 community_plugins 的种子数据。
type pluginSeedPack struct {
	id, kind, pluginID, name, description, version, license, digest string
	manifest, archive                                               []byte
	files                                                           []string
}

// uuid5URL 是 RFC 4122 第 5 版 UUID，命名空间为 URL（6ba7b811-9dad-11d1-80b4-00c04fd430c8），与 Python 的 uuid5(NAMESPACE_URL, name) 相同。
func uuid5URL(name string) string {
	h := sha1.New()
	h.Write([]byte{0x6b, 0xa7, 0xb8, 0x11, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8})
	h.Write([]byte(name))
	s := h.Sum(nil)
	s[6] = s[6]&0x0f | 0x50
	s[8] = s[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", s[0:4], s[4:6], s[6:8], s[8:10], s[10:16])
}

// RenderCommunityPluginSeed 把 source（msime-plugins 检出，或直接是其中的 packs 目录）下的每个包目录渲染成写入 community_plugins 的幂等 SQL，事务切换到运行角色 role。任何一个包不能经发布接口发布时返回错误、不输出 SQL，错误逐行列出每个被拒绝的包。
func RenderCommunityPluginSeed(source, role string) (string, error) {
	if !pluginSeedRole.MatchString(role) {
		return "", errors.New("role must be a plain lowercase PostgreSQL role name")
	}
	root := source
	if info, err := os.Stat(filepath.Join(source, "packs")); err == nil && info.IsDir() {
		root = filepath.Join(source, "packs")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	var packs []pluginSeedPack
	var refused []string
	total := 0
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		pack, err := preparePluginSeedPack(filepath.Join(root, entry.Name()))
		// msime-plugins 用目录名保证 id 唯一，目录名与清单 id 一致也就保证了种子里没有重复的 (kind, plugin_id)。
		if err == nil && pack.pluginID != entry.Name() {
			err = fmt.Errorf("invalid_plugin_manifest: manifest id %q does not match the directory name", pack.pluginID)
		}
		if err != nil {
			refused = append(refused, entry.Name()+": "+err.Error())
			continue
		}
		packs = append(packs, pack)
		total += len(pack.archive)
	}
	// 每账号配额：发布接口拒绝同一账号的第 21 个插件和超过 32 MiB 的合计包体。
	if len(packs) > maxPluginsPerUser {
		refused = append(refused, fmt.Sprintf("plugin_publish_limit: %d packs exceed the per-account limit of %d", len(packs), maxPluginsPerUser))
	}
	if total > maxPluginBytesPerUser {
		refused = append(refused, fmt.Sprintf("plugin_storage_limit: archives total %d bytes, over the per-account limit of %d", total, maxPluginBytesPerUser))
	}
	if len(refused) > 0 {
		return "", errors.New("refused " + strings.Join(refused, "\nrefused "))
	}
	if len(packs) == 0 {
		return "", fmt.Errorf("no packs found in %s", root)
	}
	return renderPluginSeed(packs, role, pluginSeedProvenance(root)), nil
}

// preparePluginSeedPack 读一个包目录、打成归档，并按发布接口的顺序校验：清单里的 kind、id、name、version 和 description 充当一次发布请求的字段，先过请求元数据的检查（pluginPublishMetadataCode），再过归档本身的检查（validPluginPublishArchive）。社区列表的标题和说明取清单的 name 和 description，按接口的规则去掉首尾空白；不合规时拒绝而不是像客户端的建议值那样截断，交给维护者在源仓库里改短。
func preparePluginSeedPack(dir string) (pluginSeedPack, error) {
	names, contents, err := collectPluginSeedFiles(dir)
	if err != nil {
		return pluginSeedPack{}, err
	}
	var request struct {
		Kind        string `toml:"kind"`
		ID          string `toml:"id"`
		Name        string `toml:"name"`
		Version     string `toml:"version"`
		Description string `toml:"description"`
	}
	manifest, ok := contents["plugin.toml"]
	if !ok {
		return pluginSeedPack{}, errors.New("invalid_plugin_manifest: plugin.toml is missing")
	}
	if err := toml.Unmarshal(manifest, &request); err != nil {
		return pluginSeedPack{}, fmt.Errorf("invalid_plugin_manifest: %v", err)
	}
	archive, err := pluginSeedArchive(names, contents)
	if err != nil {
		return pluginSeedPack{}, err
	}
	p := pluginSeedPack{
		id:          uuid5URL(fmt.Sprintf("https://msime.app/community/starter-plugin/%s/%s/%s", request.Kind, request.ID, request.Version)),
		name:        strings.TrimSpace(request.Name),
		description: strings.TrimSpace(request.Description),
		files:       names,
		archive:     archive,
	}
	if code := pluginPublishMetadataCode(p.id, p.name, p.description, request.Kind, request.ID, request.Version, len(archive)); code != "" {
		return pluginSeedPack{}, fmt.Errorf("%s: the listing takes the manifest name (1 to 32 characters) and description (at most 280), kind and id must be publishable and the archive at most %d bytes", code, maxPluginArchiveBytes)
	}
	pack, code := validPluginPublishArchive(archive, request.Kind, request.ID, request.Version)
	if code != "" {
		return pluginSeedPack{}, fmt.Errorf("%s: the server's pack validator rejects it (msime-plugins' scripts/check-packs.sh reports the client's reason)", code)
	}
	p.kind, p.pluginID, p.version, p.license, p.manifest = pack.Kind, pack.ID, pack.Version, pack.License, pack.Manifest
	p.digest = pluginRequestDigest(p.name, p.description, p.kind, p.pluginID, p.version, archive)
	return p, nil
}

// collectPluginSeedFiles 按客户端 list_files 的规则读包目录：跳过以 . 开头的名字，拒绝子目录、符号链接、非普通文件和不合规的文件名。返回按文件名字节序排列的名字和内容。
func collectPluginSeedFiles(dir string) ([]string, map[string][]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	var names []string
	contents := map[string][]byte{}
	lowered := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		switch {
		case entry.Type()&fs.ModeSymlink != 0:
			return nil, nil, fmt.Errorf("invalid_plugin_archive: %s is a symbolic link", name)
		case entry.IsDir():
			return nil, nil, fmt.Errorf("invalid_plugin_archive: %s is a subdirectory; pack files must sit at the top level", name)
		case !entry.Type().IsRegular():
			return nil, nil, fmt.Errorf("invalid_plugin_archive: %s is not a regular file", name)
		case !validPluginFileName(name):
			return nil, nil, fmt.Errorf("invalid_plugin_archive: %s is not a valid pack file name", name)
		case lowered[strings.ToLower(name)]:
			return nil, nil, fmt.Errorf("invalid_plugin_archive: %s differs from another file name only by case", name)
		case len(names) == maxPluginPackFiles:
			return nil, nil, fmt.Errorf("plugin_too_large: more than %d pack files", maxPluginPackFiles)
		}
		info, err := entry.Info()
		if err != nil {
			return nil, nil, err
		}
		if info.Size() > maxPluginFileBytes {
			return nil, nil, fmt.Errorf("plugin_too_large: %s is %d bytes, limit %d", name, info.Size(), maxPluginFileBytes)
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, nil, err
		}
		lowered[strings.ToLower(name)] = true
		names = append(names, name)
		contents[name] = data
	}
	return names, contents, nil
}

// pluginSeedArchive 把包文件放在 zip 根部，按 names 的顺序（文件名字节序）、固定时间戳和 0644 权限写出，与客户端 pack() 的布局相同。成员用 Store 而不是客户端的 Deflate：压缩结果随实现和版本变化，而重复执行种子时的不一致检查依赖归档字节可复现；服务端和客户端都接受 Store。用 CreateRaw 预先写好 CRC 和大小，本地文件头里就带着它们，不需要数据描述符。
func pluginSeedArchive(names []string, contents map[string][]byte) ([]byte, error) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, name := range names {
		data := contents[name]
		header := &zip.FileHeader{Name: name, Method: zip.Store, ReaderVersion: 20, ModifiedDate: pluginSeedModifiedDate, CRC32: crc32.ChecksumIEEE(data), CompressedSize64: uint64(len(data)), UncompressedSize64: uint64(len(data))}
		header.SetMode(0o644)
		header.CreatorVersion |= 20
		out, err := w.CreateRaw(header)
		if err != nil {
			return nil, err
		}
		if _, err = out.Write(data); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func pluginSeedLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func pluginSeedSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// renderPluginSeed 写出整个事务。packs 已按目录名排好序并通过 preparePluginSeedPack。
func renderPluginSeed(packs []pluginSeedPack, role string, source []string) string {
	owner := pluginSeedLiteral(pluginSeedPublisher)
	out := append([]string{"-- Generated by msime-server -render-plugin-seed; review before running with psql -X -v ON_ERROR_STOP=1."}, source...)
	out = append(out, "BEGIN;", "SET LOCAL ROLE "+role+";", "SET LOCAL lock_timeout = '5s';", "SET LOCAL statement_timeout = '60s';",
		"SELECT pg_advisory_xact_lock(hashtextextended("+owner+",0));",
		"INSERT INTO auth_users(id,display_name) VALUES("+owner+","+pluginSeedLiteral(pluginSeedPublisherName)+") ON CONFLICT DO NOTHING;",
		"DO $$ BEGIN IF EXISTS(SELECT 1 FROM auth_identities WHERE user_id="+owner+") OR EXISTS(SELECT 1 FROM auth_sessions WHERE user_id="+owner+") THEN RAISE EXCEPTION 'starter publisher is interactive'; END IF; END $$;",
		// moderation 列来自管理后台的迁移（admin_ops_schema.sql），缺列时先报清楚原因，而不是在插入时撞上 column does not exist。
		"DO $$ BEGIN IF NOT EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid='community_plugins'::regclass AND attname='moderation' AND NOT attisdropped) THEN RAISE EXCEPTION 'community_plugins has no moderation column; deploy the new backend or run -migrate-users first'; END IF; END $$;")
	var newer []string
	for _, p := range packs {
		if !slices.Contains(legacyPluginKinds, p.kind) && !slices.Contains(newer, p.kind) {
			newer = append(newer, p.kind)
		}
	}
	if len(newer) > 0 {
		// 辅助码表、符号集、短语表和单词本要求 kind 约束已经由新版本服务迁移过；否则先报清楚原因，而不是在插入时撞上约束。
		slices.Sort(newer)
		probes := make([]string, len(newer))
		for i, kind := range newer {
			probes[i] = "pg_get_constraintdef(oid) LIKE '%''" + kind + "''%'"
		}
		out = append(out, "DO $$ BEGIN IF NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='community_plugins'::regclass AND conname='community_plugins_kind_known' AND "+strings.Join(probes, " AND ")+") THEN RAISE EXCEPTION 'community_plugins kind constraint predates "+strings.Join(newer, ", ")+"; deploy the new backend or run -migrate-users first'; END IF; END $$;")
	}
	wanted := make([]string, len(packs))
	for i, p := range packs {
		id, kind, pluginID, name, description, version, license := pluginSeedLiteral(p.id), pluginSeedLiteral(p.kind), pluginSeedLiteral(p.pluginID), pluginSeedLiteral(p.name), pluginSeedLiteral(p.description), pluginSeedLiteral(p.version), pluginSeedLiteral(p.license)
		archiveSum, manifestSum := pluginSeedSHA256(p.archive), pluginSeedSHA256(p.manifest)
		out = append(out,
			fmt.Sprintf("-- Pack %s (%s) %s: archive %d bytes, sha256 %s, files %s", p.pluginID, p.kind, p.version, len(p.archive), archiveSum, strings.Join(p.files, ", ")),
			"INSERT INTO community_plugins(id,owner_id,kind,plugin_id,name,description,version,license,manifest,archive,request_sha256,moderation) VALUES("+
				strings.Join([]string{id, owner, kind, pluginID, name, description, version, license, "decode('" + hex.EncodeToString(p.manifest) + "','hex')", "decode('" + hex.EncodeToString(p.archive) + "','hex')", "'" + p.digest + "'", "'approved'"}, ",")+
				") ON CONFLICT(id) DO NOTHING;",
			// 不比较 moderation：运维下架后重新执行种子不应失败，也不会把作品恢复上架。
			"DO $$ BEGIN IF NOT EXISTS(SELECT 1 FROM community_plugins WHERE id="+id+" AND owner_id="+owner+" AND kind="+kind+" AND plugin_id="+pluginID+" AND name="+name+" AND description="+description+" AND version="+version+" AND license="+license+
				" AND encode(sha256(manifest),'hex')='"+manifestSum+"' AND sha256='"+archiveSum+"' AND request_sha256='"+p.digest+"') THEN RAISE EXCEPTION 'starter plugin % differs from the reviewed pack; publish a new version instead of overwriting', "+id+"; END IF; END $$;")
		wanted[i] = "(" + kind + "," + pluginID + ")"
	}
	// 客户端按 (kind, plugin_id) 安装，同名的用户作品会与精选包互相替换；列出来交给运维判断。
	out = append(out, "SELECT p.kind,p.plugin_id,p.version,p.moderation,p.owner_id="+owner+" AS starter FROM community_plugins p WHERE (p.kind,p.plugin_id) IN (VALUES "+strings.Join(wanted, ",")+") ORDER BY p.kind,p.plugin_id,starter DESC,p.created_at;", "COMMIT;")
	return strings.Join(out, "\n") + "\n"
}

// pluginSeedProvenance 在 SQL 开头记录来源仓库（去掉凭据）和提交；packs 目录有未提交改动时给出警告。
func pluginSeedProvenance(root string) []string {
	git := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...).Output()
		return strings.TrimSpace(string(out)), err
	}
	commit, err := git("rev-parse", "HEAD")
	if err != nil {
		return []string{"-- Source: not a git checkout"}
	}
	remote, err := git("remote", "get-url", "origin")
	if err != nil {
		remote = "(no origin remote)"
	} else if u, err := url.Parse(remote); err == nil && u.User != nil {
		u.User = nil
		remote = u.String()
	}
	lines := []string{"-- Source: " + remote + " at commit " + commit}
	if status, err := git("status", "--porcelain", "--untracked-files=all", "--", "."); err != nil || status != "" {
		lines = append(lines, "-- WARNING: the checkout has uncommitted changes; this SQL reflects the files on disk, not the commit above.")
	}
	return lines
}
