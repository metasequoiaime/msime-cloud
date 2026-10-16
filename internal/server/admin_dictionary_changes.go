package server

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

var adminPackName = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,63}$`)
var dictionarySHAPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

type dictionaryEdit struct {
	Operation string   `json:"operation"`
	Line      int      `json:"line"`
	Columns   []string `json:"columns"`
}

type dictionaryChangeRequest struct {
	Path    string           `json:"path"`
	SHA     string           `json:"sha"`
	Changes []dictionaryEdit `json:"changes"`
}

// 变更只写入新分支并开 PR；custom 保持只追加，sources 仍按上游字节锁定。
func (s *Server) dictionaryChanges(w http.ResponseWriter, r *http.Request) {
	if !requirePerm(w, r, account.PermReviewDictPR) {
		return
	}
	gh, ok := s.adminGitHubApp(w)
	if !ok {
		return
	}
	var input dictionaryChangeRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		fail(w, 400, "invalid_request")
		return
	}
	if decoder.Decode(new(any)) != io.EOF {
		fail(w, 400, "invalid_request")
		return
	}
	if !dictionaryEditablePath(input.Path) || !dictionaryTextPath(input.Path) {
		fail(w, 400, "dictionary_readonly")
		return
	}
	if !dictionarySHAPattern.MatchString(input.SHA) || len(input.Changes) < 1 || len(input.Changes) > 100 {
		fail(w, 400, "invalid_dictionary_changes")
		return
	}
	result, err := s.createDictionaryPR(r.Context(), gh, input)
	if err != nil {
		var rejected *dictionaryChangeError
		if errors.As(err, &rejected) {
			fail(w, rejected.status, rejected.code)
		} else {
			dictGitHubError(w, err)
		}
		return
	}
	if s.accounts != nil {
		if err := s.accounts.Audit(r.Context(), "dictionary_change", input.Path, map[string]any{"repo": s.dictRepo(), "pull": result.Number, "count": len(input.Changes)}); err != nil {
			slog.Error("dictionary management: audit unavailable", "pull", result.Number)
			respond(w, 503, map[string]any{"error": map[string]string{"code": "dictionary_not_recorded", "message": "PR 已创建，但审计暂未写入。"}, "pull_url": result.URL})
			return
		}
	}
	respond(w, 200, result)
}

type dictionaryChangeError struct {
	status int
	code   string
}

func (e *dictionaryChangeError) Error() string           { return e.code }
func dictionaryChangeFail(status int, code string) error { return &dictionaryChangeError{status, code} }

type dictionaryChangeResult struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	Branch string `json:"branch"`
}

func (s *Server) createDictionaryPR(ctx context.Context, gh *githubapp.Client, input dictionaryChangeRequest) (dictionaryChangeResult, error) {
	var result dictionaryChangeResult
	repo := s.dictRepo()
	token, err := gh.Token(ctx, repo, dictPRPermissions)
	if err != nil {
		return result, err
	}
	root := "/repos/" + repo
	read := func(p string, out any) error {
		res, err := gh.Do(ctx, token, "GET", root+p, nil)
		if err != nil {
			return err
		}
		if !res.OK() {
			return dictionaryChangeFail(503, "github_unavailable")
		}
		if res.Decode(out) != nil {
			return dictionaryChangeFail(503, "github_unavailable")
		}
		return nil
	}
	var meta struct {
		Branch string `json:"default_branch"`
	}
	if err := read("", &meta); err != nil {
		return result, err
	}
	if meta.Branch == "" {
		return result, dictionaryChangeFail(503, "github_unavailable")
	}
	var base struct {
		SHA string `json:"sha"`
	}
	if err := read("/commits/"+url.PathEscape(meta.Branch), &base); err != nil {
		return result, err
	}
	if !dictionarySHAPattern.MatchString(base.SHA) {
		return result, dictionaryChangeFail(503, "github_unavailable")
	}
	raw, err := gh.Raw(ctx, token, root+"/contents/"+input.Path+"?"+url.Values{"ref": {base.SHA}}.Encode(), 4<<20)
	if err != nil {
		return result, err
	}
	if !raw.OK() {
		return result, dictionaryChangeFail(503, "github_unavailable")
	}
	if dictionaryBlobSHA(raw.Body) != input.SHA {
		return result, dictionaryChangeFail(409, "dictionary_changed")
	}
	text := string(raw.Body)
	next, err := applyDictionaryChanges(input.Path, text, input.Changes)
	if err != nil {
		return result, err
	}
	if next == text {
		return result, dictionaryChangeFail(400, "dictionary_no_changes")
	}
	branch := "dictionary-admin/" + adminRandom()
	result.Branch = branch
	// 每次请求创建随机分支，多副本无需共享进程锁；所有修改基于同一不可变提交。
	write := func(method, p string, body, out any) error {
		res, err := gh.Do(ctx, token, method, root+p, body)
		if err != nil || res.Status >= 500 {
			return dictionaryChangeFail(502, "dictionary_uncertain")
		}
		if !res.OK() {
			return dictionaryChangeFail(502, "github_error")
		}
		if out != nil && res.Decode(out) != nil {
			return dictionaryChangeFail(502, "dictionary_uncertain")
		}
		return nil
	}
	if err := write("POST", "/git/refs", map[string]any{"ref": "refs/heads/" + branch, "sha": base.SHA}, nil); err != nil {
		return result, err
	}
	title := "feat(dictionary): 更新 " + input.Path + " 的词条"
	if err := write("PUT", "/contents/"+input.Path, map[string]any{"message": title, "content": base64.StdEncoding.EncodeToString([]byte(next)), "sha": input.SHA, "branch": branch}, nil); err != nil {
		return result, err
	}
	var pull struct {
		Number int    `json:"number"`
		URL    string `json:"html_url"`
	}
	body := fmt.Sprintf("通过水杉管理后台提交 %d 项词条变更。\n\n文件：`%s`\n基准版本：`%s`\n\n请检查格式、读音、权重、重复项与翻译覆盖，并等待仓库检查通过后合入。", len(input.Changes), input.Path, base.SHA)
	if err := write("POST", "/pulls", map[string]any{"title": title, "head": branch, "base": meta.Branch, "body": body}, &pull); err != nil {
		return result, err
	}
	if pull.Number <= 0 || pull.URL != "https://github.com/"+repo+"/pull/"+strconv.Itoa(pull.Number) {
		return result, dictionaryChangeFail(502, "dictionary_uncertain")
	}
	gh.Invalidate("/repos/" + repo + "/pulls")
	result.Number, result.URL = pull.Number, pull.URL
	return result, nil
}

func dictionaryBlobSHA(raw []byte) string {
	h := sha1.New()
	_, _ = fmt.Fprintf(h, "blob %d%c", len(raw), 0)
	_, _ = h.Write(raw)
	return hex.EncodeToString(h.Sum(nil))
}

func applyDictionaryChanges(name, text string, changes []dictionaryEdit) (string, error) {
	if !dictionaryEditablePath(name) || !utf8.ValidString(text) {
		return "", dictionaryChangeFail(400, "dictionary_readonly")
	}
	lines := strings.Split(text, "\n")
	validator := dictionaryColumnValidator{name: name, lines: lines}
	newline := "\n"
	if strings.Contains(text, "\r\n") {
		newline = "\r\n"
	}
	updates := map[int]*string{}
	additions := []string{}
	for _, edit := range changes {
		if !slices.Contains([]string{"add", "replace", "delete"}, edit.Operation) || (edit.Operation != "add" && strings.HasPrefix(name, "custom/")) {
			return "", dictionaryChangeFail(400, "invalid_dictionary_changes")
		}
		if edit.Operation == "add" {
			if edit.Line != 0 {
				return "", dictionaryChangeFail(400, "invalid_dictionary_changes")
			}
		} else {
			if edit.Line < 1 || edit.Line > len(lines) || strings.TrimSpace(lines[edit.Line-1]) == "" || strings.HasPrefix(strings.TrimSpace(lines[edit.Line-1]), "#") {
				return "", dictionaryChangeFail(400, "invalid_dictionary_changes")
			}
			if _, exists := updates[edit.Line]; exists {
				return "", dictionaryChangeFail(400, "invalid_dictionary_changes")
			}
		}
		if edit.Operation == "delete" {
			if len(edit.Columns) != 0 {
				return "", dictionaryChangeFail(400, "invalid_dictionary_changes")
			}
			updates[edit.Line] = nil
			continue
		}
		if err := validator.validate(edit.Columns); err != nil {
			return "", err
		}
		value := strings.Join(edit.Columns, "\t")
		if edit.Operation == "add" {
			additions = append(additions, value)
		} else {
			updates[edit.Line] = &value
		}
	}
	// 按删改后的字节数预留空间，追加前可能需要补一个换行。
	outputBytes := len(text)
	for line, value := range updates {
		original := lines[line-1]
		if value == nil {
			outputBytes -= len(original)
			if line < len(lines) {
				outputBytes--
			}
		} else {
			outputBytes += len(*value) - len(original)
			if strings.HasSuffix(original, "\r") {
				outputBytes++
			}
		}
	}
	if outputBytes > 0 && len(additions) > 0 {
		outputBytes += len(newline)
	}
	for _, value := range additions {
		outputBytes += len(value) + len(newline)
	}
	// 保留未修改行的每一个字节及换行风格，仅修改目标行。
	var out strings.Builder
	out.Grow(outputBytes)
	for i, original := range lines {
		value, changed := updates[i+1]
		if changed && value == nil {
			continue
		}
		if changed {
			out.WriteString(*value)
			if strings.HasSuffix(original, "\r") {
				out.WriteByte('\r')
			}
		} else {
			out.WriteString(original)
		}
		if i < len(lines)-1 {
			out.WriteByte('\n')
		}
	}
	if len(additions) > 0 {
		if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
			out.WriteString(newline)
		}
		for _, value := range additions {
			out.WriteString(value)
			out.WriteString(newline)
		}
	}
	result := out.String()
	// 翻译允许覆盖同一源词，其他文件按前两列去重。
	remaining := map[string]int{}

	for _, line := range lines {
		if k := dictionaryEntryKey(line); k != "" {
			remaining[k]++
		}
	}
	for line := range strings.SplitSeq(result, "\n") {
		k := dictionaryEntryKey(line)
		if k == "" {
			continue
		}
		if count, exists := remaining[k]; !exists {
			// 新键只允许出现一次；保留零值以拒绝后续重复。
			remaining[k] = 0
		} else if count > 0 {
			remaining[k] = count - 1
		} else {
			return "", dictionaryChangeFail(409, "dictionary_duplicate")
		}
	}
	return result, nil
}

// dictionaryEntryKey 从原行截取前两列作为去重键，避免拆分再拼接。
func dictionaryEntryKey(line string) string {
	line = strings.TrimSuffix(line, "\r")
	first, rest, ok := strings.Cut(line, "\t")
	if !ok {
		return ""
	}
	second, _, _ := strings.Cut(rest, "\t")
	return line[:len(first)+1+len(second)]
}

// dictionaryColumnValidator 在单次变更中复用原词库的权重范围，避免每条新增词条都重扫文件。
type dictionaryColumnValidator struct {
	name        string
	lines       []string
	weightsRead bool
	minimum     int64
	maximum     int64
}

func (v *dictionaryColumnValidator) validate(cols []string) error {
	name := v.name
	invalid := func() error { return dictionaryChangeFail(400, "invalid_dictionary_entry") }
	isTranslation := path.Base(name) == "translations.txt"
	required := 3
	if isTranslation {
		required = 2
	}
	if len(cols) != required {
		return invalid()
	}
	for _, field := range cols {
		if field == "" || strings.TrimSpace(field) != field || !plainText(field) || len(field) > 1000 || strings.HasPrefix(field, "#") {
			return invalid()
		}
	}
	if isTranslation {
		if len(validateTranslationEntries([]translationSubmissionEntry{{Source: cols[0], Gloss: cols[1]}})) > 0 {
			return invalid()
		}
		return nil
	}
	weight, err := strconv.ParseInt(cols[2], 10, 64)
	if err != nil || weight < 1 {
		return invalid()
	}
	if path.Base(name) == "english.txt" {
		if strings.HasPrefix(name, "packs/") {
			if len(cols[0]) > 64 || strings.IndexFunc(cols[0], func(r rune) bool { return !(r >= 'a' && r <= 'z' || r == '-' || r == '\'') }) >= 0 || weight > 10 {
				return invalid()
			}
		} else if len(validateEnglishEntries([]englishSubmissionEntry{{Word: cols[0], Display: cols[1]}})) > 0 {
			return invalid()
		}
	} else if len(validateWordEntries([]wordSubmissionEntry{{Word: cols[0], Pinyin: cols[1]}})) > 0 || weight > 10000 {
		return invalid()
	}
	if strings.HasPrefix(name, "custom/") {
		if !v.weightsRead {
			for _, line := range v.lines {
				first, rest, ok := strings.Cut(strings.TrimSpace(line), "\t")
				if !ok || strings.HasPrefix(first, "#") {
					continue
				}
				_, weightField, ok := strings.Cut(rest, "\t")
				if !ok || strings.Contains(weightField, "\t") {
					continue
				}
				n, err := strconv.ParseInt(weightField, 10, 64)
				if err != nil {
					continue
				}
				if v.minimum == 0 || n < v.minimum {
					v.minimum = n
				}
				if n > v.maximum {
					v.maximum = n
				}
			}
			v.weightsRead = true
		}
		if v.minimum == 0 || weight < v.minimum || weight > v.maximum {
			return invalid()
		}
	}
	return nil
}
