package server

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

type dictionarySearchFile struct {
	Path     string          `json:"path"`
	Rows     []dictionaryRow `json:"rows"`
	Total    int             `json:"total"`
	Editable bool            `json:"editable"`
}

// 每次最多搜索四个文件，批次按 8 MiB 控制，大文件单独处理；各批次固定同一不可变树。
func (s *Server) dictionarySearch(w http.ResponseWriter, r *http.Request, gh *githubapp.Client) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = "all"
	}
	revision := r.URL.Query().Get("revision")
	cursor := 0
	var err error
	if value := r.URL.Query().Get("cursor"); value != "" {
		cursor, err = strconv.Atoi(value)
	}
	if q == "" || len(q) > 200 || !utf8.ValidString(q) || !plainText(r.URL.Query().Get("q")) ||
		!slices.Contains([]string{"all", "custom", "packs", "sources"}, scope) || err != nil || cursor < 0 ||
		(revision != "" && !dictionarySHAPattern.MatchString(revision)) || (cursor > 0 && revision == "") {
		fail(w, 400, "invalid_query")
		return
	}
	repo := s.dictRepo()
	token, err := gh.Token(r.Context(), repo, dictionaryReadPermissions)
	if err != nil {
		dictGitHubError(w, err)
		return
	}
	if revision == "" {
		res, err := gh.Get(r.Context(), token, "/repos/"+repo)
		if err != nil {
			dictGitHubError(w, err)
			return
		}
		var meta struct {
			Branch string `json:"default_branch"`
		}
		if !res.OK() || res.Decode(&meta) != nil || meta.Branch == "" {
			fail(w, 503, "github_unavailable")
			return
		}
		revision = meta.Branch
	}
	res, err := gh.Get(r.Context(), token, "/repos/"+repo+"/git/trees/"+url.PathEscape(revision)+"?recursive=1")
	if err != nil {
		dictGitHubError(w, err)
		return
	}
	var tree struct {
		SHA       string               `json:"sha"`
		Tree      []dictionaryTreeItem `json:"tree"`
		Truncated bool                 `json:"truncated"`
	}
	if !res.OK() || res.Decode(&tree) != nil || !dictionarySHAPattern.MatchString(tree.SHA) {
		fail(w, 503, "github_unavailable")
		return
	}
	files := make([]dictionaryTreeItem, 0, len(tree.Tree))
	for _, item := range tree.Tree {
		if item.Type == "blob" && dictionaryTextPath(item.Path) && (scope == "all" || strings.HasPrefix(item.Path, scope+"/")) {
			files = append(files, item)
		}
	}
	slices.SortFunc(files, func(a, b dictionaryTreeItem) int { return strings.Compare(a.Path, b.Path) })
	if cursor > len(files) {
		fail(w, 400, "invalid_query")
		return
	}
	end := cursor
	var bytes int64
	results := []dictionarySearchFile{}
	skipped := []map[string]string{}
	for _, file := range files[cursor:min(cursor+4, len(files))] {
		if file.Size <= 64<<20 && end > cursor && file.Size > (8<<20)-bytes {
			break
		}
		end++
		if file.Size > 64<<20 {
			skipped = append(skipped, map[string]string{"path": file.Path, "reason": "文件超过 64 MiB"})
			continue
		}
		bytes += max(file.Size, 0)
		raw, err := gh.Raw(r.Context(), token, "/repos/"+repo+"/git/blobs/"+url.PathEscape(file.SHA), 64<<20)
		if err != nil {
			dictGitHubError(w, err)
			return
		}
		if !raw.OK() {
			dictGitHubStatus(w, "dictionary search blob", raw.Status)
			return
		}
		text, _, err := decodeDictionaryText(raw.Body)
		if err != nil {
			skipped = append(skipped, map[string]string{"path": file.Path, "reason": "文件编码无法识别"})
			continue
		}
		rows, total := dictionaryRows(file.Path, text, q, 1)
		if total > 0 {
			results = append(results, dictionarySearchFile{Path: file.Path, Rows: rows, Total: total, Editable: dictionaryEditablePath(file.Path)})
		}
	}
	respond(w, 200, map[string]any{"files": results, "skipped": skipped, "next_cursor": end, "total_files": len(files), "has_more": end < len(files), "revision": tree.SHA, "truncated": tree.Truncated})
}
