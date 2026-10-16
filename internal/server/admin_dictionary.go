package server

import (
	"bytes"
	"encoding/csv"
	"errors"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
	textunicode "golang.org/x/text/encoding/unicode"
)

// 词库管理从配置的仓库读取文本，不向浏览器暴露 GitHub 凭据。
var dictionaryReadPermissions = map[string]string{"contents": "read"}

type dictionaryTreeItem struct {
	Path string `json:"path"`
	Type string `json:"type"`
	Size int64  `json:"size"`
	SHA  string `json:"sha"`
	URL  string `json:"url,omitempty"`
}

func (s *Server) adminDictionary(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/dictionary/changes" && r.Method == http.MethodPost {
		s.dictionaryChanges(w, r)
		return
	}
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	gh, ok := s.adminGitHubApp(w)
	if !ok {
		return
	}
	if r.URL.Path == "/api/dictionary/file" {
		s.dictionaryFile(w, r, gh)
		return
	}
	if r.URL.Path == "/api/dictionary/search" {
		s.dictionarySearch(w, r, gh)
		return
	}
	if r.URL.Path != "/api/dictionary" {
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	s.dictionaryTree(w, r, gh)
}

func (s *Server) dictionaryTree(w http.ResponseWriter, r *http.Request, gh *githubapp.Client) {
	repo := s.dictRepo()
	token, err := gh.Token(r.Context(), repo, dictionaryReadPermissions)
	if err != nil {
		dictGitHubError(w, err)
		return
	}
	meta, err := gh.Get(r.Context(), token, "/repos/"+repo)
	if err != nil {
		dictGitHubError(w, err)
		return
	}
	if !meta.OK() {
		dictGitHubStatus(w, "repository metadata", meta.Status)
		return
	}
	var repository struct {
		DefaultBranch string `json:"default_branch"`
		HTMLURL       string `json:"html_url"`
		Description   string `json:"description"`
		PushedAt      string `json:"pushed_at"`
	}
	if err := meta.Decode(&repository); err != nil || repository.DefaultBranch == "" {
		fail(w, http.StatusServiceUnavailable, "github_unavailable")
		return
	}
	query := url.Values{"recursive": {"1"}}
	tree, err := gh.Get(r.Context(), token, "/repos/"+repo+"/git/trees/"+url.PathEscape(repository.DefaultBranch)+"?"+query.Encode())
	if err != nil {
		dictGitHubError(w, err)
		return
	}
	if !tree.OK() {
		dictGitHubStatus(w, "dictionary tree", tree.Status)
		return
	}
	var result struct {
		Tree      []dictionaryTreeItem `json:"tree"`
		Truncated bool                 `json:"truncated"`
	}
	if err := tree.Decode(&result); err != nil {
		fail(w, http.StatusServiceUnavailable, "github_unavailable")
		return
	}
	files := make([]dictionaryTreeItem, 0, len(result.Tree))
	for _, item := range result.Tree {
		if item.Type != "blob" || !dictionaryTextPath(item.Path) {
			continue
		}
		files = append(files, item)
	}
	slices.SortFunc(files, func(a, b dictionaryTreeItem) int { return strings.Compare(a.Path, b.Path) })
	counts := map[string]int{"custom": 0, "packs": 0, "sources": 0}
	var bytes int64
	for _, item := range files {
		prefix, _, _ := strings.Cut(item.Path, "/")
		counts[prefix]++
		bytes += item.Size
	}
	respond(w, http.StatusOK, map[string]any{
		"repo": repo, "branch": repository.DefaultBranch, "url": repository.HTMLURL,
		"description": repository.Description, "pushed_at": repository.PushedAt,
		"files": files, "truncated": result.Truncated, "counts": counts, "bytes": bytes,
	})
}

func (s *Server) dictionaryFile(w http.ResponseWriter, r *http.Request, gh *githubapp.Client) {
	query := r.URL.Query()
	name := strings.TrimSpace(query.Get("path"))
	if !dictionaryTextPath(name) || len(name) > 256 || !utf8.ValidString(name) || strings.Contains(name, "..") {
		fail(w, http.StatusBadRequest, "invalid_dictionary_path")
		return
	}
	page := 1
	if value := query.Get("page"); value != "" {
		parsed, err := strconv.Atoi(value)
		page = parsed
		if err != nil || page < 1 || page > 1000000 {
			fail(w, 400, "invalid_page")
			return
		}
	}
	q := query.Get("q")
	if len(q) > 200 || !plainText(q) {
		fail(w, 400, "invalid_query")
		return
	}
	repo := s.dictRepo()
	token, err := gh.Token(r.Context(), repo, dictionaryReadPermissions)
	if err != nil {
		dictGitHubError(w, err)
		return
	}
	meta, err := gh.Get(r.Context(), token, "/repos/"+repo)
	if err != nil {
		dictGitHubError(w, err)
		return
	}
	if !meta.OK() {
		dictGitHubStatus(w, "repository metadata", meta.Status)
		return
	}
	var repository struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := meta.Decode(&repository); err != nil || repository.DefaultBranch == "" {
		fail(w, http.StatusServiceUnavailable, "github_unavailable")
		return
	}
	tree, err := gh.Get(r.Context(), token, "/repos/"+repo+"/git/trees/"+url.PathEscape(repository.DefaultBranch)+"?recursive=1")
	if err != nil {
		dictGitHubError(w, err)
		return
	}
	if !tree.OK() {
		dictGitHubStatus(w, "dictionary tree", tree.Status)
		return
	}
	var listing struct {
		Tree []dictionaryTreeItem `json:"tree"`
	}
	if err := tree.Decode(&listing); err != nil {
		fail(w, http.StatusServiceUnavailable, "github_unavailable")
		return
	}
	var item *dictionaryTreeItem
	for i := range listing.Tree {
		if listing.Tree[i].Path == name && listing.Tree[i].Type == "blob" {
			item = &listing.Tree[i]
			break
		}
	}
	if item == nil {
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	fileURL := "https://github.com/" + repo + "/blob/" + url.PathEscape(repository.DefaultBranch) + "/" + name
	raw, err := gh.Raw(r.Context(), token, "/repos/"+repo+"/git/blobs/"+url.PathEscape(item.SHA), 64<<20)
	if err != nil {
		dictGitHubError(w, err)
		return
	}
	if !raw.OK() {
		dictGitHubStatus(w, "dictionary blob", raw.Status)
		return
	}
	text, encoding, err := decodeDictionaryText(raw.Body)
	if err != nil {
		fail(w, 502, "dictionary_encoding")
		return
	}
	rows, total := dictionaryRows(name, text, q, page)
	respond(w, http.StatusOK, map[string]any{"repo": repo, "name": path.Base(name), "path": name, "sha": item.SHA, "size": item.Size, "url": fileURL, "rows": rows, "total": total, "page": page, "has_more": total > page*100, "encoding": encoding, "editable": dictionaryEditablePath(name)})

}

func dictionaryTextPath(name string) bool {
	if name == "" || path.IsAbs(name) || path.Clean(name) != name || strings.ContainsAny(name, "\\\r\n\t") {
		return false
	}
	prefix, rest, _ := strings.Cut(name, "/")
	if prefix == "custom" {
		return slices.Contains([]string{"custom/words.txt", "custom/english.txt", "custom/translations.txt", "custom/names.txt"}, name)
	}
	if prefix == "packs" {
		return dictionaryEditablePath(name)
	}
	if prefix != "sources" {
		return false
	}
	source, file, ok := strings.Cut(rest, "/")
	if !ok || strings.Contains(file, "/") {
		return false
	}
	base := strings.ToLower(file)
	stem, _, _ := strings.Cut(base, ".")
	if slices.Contains([]string{"readme", "license", "licence", "notice", "changelog", "agents"}, stem) {
		return false
	}
	// 日文目录同时包含连接代价矩阵、词性编号和过滤规则，仅展示实际词条表。
	if source == "japanese" {
		if slices.Contains([]string{"aux_dictionary.tsv", "places.tsv", "words.tsv"}, file) {
			return true
		}
		return len(file) == len("dictionary00.txt") && strings.HasPrefix(file, "dictionary") &&
			file[10] >= '0' && file[10] <= '9' && file[11] >= '0' && file[11] <= '9' && strings.HasSuffix(file, ".txt")
	}
	ext := path.Ext(base)
	return ext == ".txt" || ext == ".tsv" || ext == ".csv" || ext == ".occ" || strings.HasSuffix(base, ".dict.yaml")
}

// 原始数据保留字节；解码只用于预览，不把 UTF-16 或换行转换后的内容写回仓库。
func decodeDictionaryText(raw []byte) (string, string, error) {
	if bytes.HasPrefix(raw, []byte{0xff, 0xfe}) || bytes.HasPrefix(raw, []byte{0xfe, 0xff}) {
		endian := textunicode.LittleEndian
		if bytes.HasPrefix(raw, []byte{0xfe, 0xff}) {
			endian = textunicode.BigEndian
		}
		out, err := textunicode.UTF16(endian, textunicode.ExpectBOM).NewDecoder().Bytes(raw)
		return string(out), "UTF-16", err
	}
	if !utf8.Valid(raw) {
		return "", "", errors.New("unsupported dictionary encoding")
	}
	return strings.TrimPrefix(string(raw), "\ufeff"), "UTF-8", nil
}

type dictionaryRow struct {
	Line    int      `json:"line"`
	Columns []string `json:"columns"`
}

func dictionaryRows(name, text, query string, page int) ([]dictionaryRow, int) {
	rows := []dictionaryRow{}
	total := 0
	q := strings.ToLower(query)
	yamlHeader := strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
	lineNumber := 0
	for raw := range strings.SplitSeq(text, "\n") {
		lineNumber++
		line := strings.TrimSpace(raw)
		if yamlHeader {
			if line == "..." {
				yamlHeader = false
			}
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "//") || (q != "" && !dictionaryLineContainsLower(line, q)) {
			continue
		}
		total++
		if total <= (page-1)*100 || total > page*100 {
			continue
		}
		var cols []string
		if strings.HasSuffix(name, ".csv") {
			reader := csv.NewReader(strings.NewReader(line))
			reader.FieldsPerRecord = -1
			if parsed, err := reader.Read(); err == nil {
				cols = parsed
			} else {
				cols = strings.Split(strings.TrimSuffix(raw, "\r"), "\t")
			}
		} else if strings.HasPrefix(name, "sources/korean/") {
			cols = strings.SplitN(line, ":", 3)
		} else {
			cols = strings.Split(strings.TrimSuffix(raw, "\r"), "\t")
			if len(cols) == 1 && !strings.HasSuffix(name, ".md") {
				cols = strings.Fields(line)
			}
		}
		rows = append(rows, dictionaryRow{Line: lineNumber, Columns: cols})
	}
	return rows, total
}

func dictionaryLineContainsLower(line, query string) bool {
	// 短词条在栈上转小写，长行分段转换并搜索。
	var lower [256]byte
	if len(line) > len(lower) {
		return dictionaryLongLineContainsLower(line, query)
	}
	for i := range len(line) {
		c := line[i]
		if c >= utf8.RuneSelf {
			return dictionaryUnicodeLineContainsLower(line, query)
		}
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		lower[i] = c
	}
	if len(query) <= 32 {
		return bytes.Contains(lower[:len(line)], []byte(query))
	}
	return dictionaryShortBytesContain(lower[:len(line)], query)
}

// dictionaryUnicodeLineContainsLower 处理最多 256 字节的短行；每个输入字节最多映射为四字节编码。
func dictionaryUnicodeLineContainsLower(line, query string) bool {
	for i, c := range line {
		r := unicode.ToLower(c)
		if r == c && c != utf8.RuneError {
			continue
		}
		_, size := utf8.DecodeRuneInString(line[i:])
		if r == c && size != 1 {
			continue
		}
		var lower [256 * utf8.UTFMax]byte
		folded := lower[:copy(lower[:], line[:i])]
		folded = utf8.AppendRune(folded, r)
		for _, rest := range line[i+size:] {
			folded = utf8.AppendRune(folded, unicode.ToLower(rest))
		}
		if len(query) <= 32 {
			return bytes.Contains(folded, []byte(query))
		}
		return dictionaryShortBytesContain(folded, query)
	}
	return strings.Contains(line, query)
}

// dictionaryShortBytesContain 搜索最多 1024 字节的短行结果，避免长查询转换成字节切片时分配。
func dictionaryShortBytesContain(folded []byte, query string) bool {
	if len(query) > len(folded) {
		return false
	}
	var buffer [256 * utf8.UTFMax]byte
	return bytes.Contains(folded, buffer[:copy(buffer[:], query)])
}

// dictionaryLongLineContainsLower 找到首个需要转换的字符，不变的长行直接搜索原文。
func dictionaryLongLineContainsLower(line, query string) bool {
	// 接口查询最多 200 字节，即使每字节转为四字节编码也在此范围内。
	if len(query) > 1024 {
		return strings.Contains(strings.ToLower(line), query)
	}
	if query == "" {
		return true
	}
	for i := range len(line) {
		c := line[i]
		if c >= utf8.RuneSelf {
			for offset, r := range line[i:] {
				if unicode.ToLower(r) != r {
					return dictionaryFoldedLineContains(line, query, i+offset)
				}
				if r == utf8.RuneError {
					if _, size := utf8.DecodeRuneInString(line[i+offset:]); size == 1 {
						return dictionaryFoldedLineContains(line, query, i+offset)
					}
				}
			}
			return strings.Contains(line, query)
		}
		if 'A' <= c && c <= 'Z' {
			return dictionaryFoldedLineContains(line, query, i)
		}
	}
	return strings.Contains(line, query)
}

// dictionaryFoldedLineContains 按固定大小的段搜索小写结果，保留查询长度减一的尾部以覆盖跨段匹配。
func dictionaryFoldedLineContains(line, query string, start int) bool {
	prefix := line[:start]
	if strings.Contains(prefix, query) {
		return true
	}
	var buffer [2048]byte
	var queryBuffer [1024]byte
	needle := queryBuffer[:copy(queryBuffer[:], query)]
	overlap := min(len(prefix), len(query)-1)
	folded := buffer[:copy(buffer[:], prefix[len(prefix)-overlap:])]
	for _, c := range line[start:] {
		if len(folded) > len(buffer)-utf8.UTFMax {
			if bytes.Contains(folded, needle) {
				return true
			}
			overlap = len(query) - 1
			copy(buffer[:], folded[len(folded)-overlap:])
			folded = buffer[:overlap]
		}
		folded = utf8.AppendRune(folded, unicode.ToLower(c))
	}
	return bytes.Contains(folded, needle)
}

func dictionaryEditablePath(name string) bool {
	if slices.Contains([]string{"custom/words.txt", "custom/english.txt", "custom/translations.txt"}, name) {
		return true
	}
	prefix, rest, ok := strings.Cut(name, "/")
	if !ok || prefix != "packs" {
		return false
	}
	pack, file, ok := strings.Cut(rest, "/")
	return ok && !strings.Contains(file, "/") && adminPackName.MatchString(pack) && slices.Contains([]string{"quanpin.txt", "english.txt", "translations.txt"}, file)
}
