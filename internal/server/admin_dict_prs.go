package server

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

// Dictionary pull request review (unit U1): the community-words/ pull requests on admin.github.dictionary_repo.

const (
	// dictPRListSize is how many of the most recent pull requests the console reads; website submissions share one rolling pull request at a time, so this covers months of history.
	dictPRListSize = 100
	// dictPRCountedOpen bounds how many open pull requests the list enriches with entry counts; normally at most one rolling pull request is open.
	dictPRCountedOpen = 5
	// dictPREngineBatch keeps each Engine lookup well under its 64 KiB request limit.
	dictPREngineBatch = 300
	dictPRReasonChars = 500
	dictPRMaxKeep     = 5000
)

// Entry check results. A sensitive-word hit outranks an entry the website form would reject, which outranks a duplicate.
const (
	dictFlagNew = "new"
	dictFlagDup = "dup"
	dictFlagAd  = "ad"
	dictFlagBad = "bad"
)

// dictPRPermissions is the installation token scope for every dictionary pull request call: reading and rewriting files on the branch, and commenting on, closing and merging the pull request.
var dictPRPermissions = map[string]string{"contents": "write", "pull_requests": "write"}

var errDictPRConflict = errors.New("dictionary pull request changed concurrently")

// dictPRState 保存本副本审核页的列表、投稿说明搜索索引，以及 NotifyOnce 前的通知缓存。
type dictPRState struct {
	once     sync.Once
	mu       sync.Mutex
	index    []dictPRSummary
	notes    map[int]string
	notified map[int]bool
}

// dictPRState returns the Server's review page state, creating its maps on first use; it lives in the Server, so it goes away with it.
func (s *Server) dictPRState() *dictPRState {
	st := &s.dictPRs
	st.once.Do(func() {
		st.notes, st.notified = map[int]string{}, map[int]bool{}
	})
	return st
}

type dictPull struct {
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	State     string     `json:"state"`
	HTMLURL   string     `json:"html_url"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	MergedAt  *time.Time `json:"merged_at"`
	Mergeable *bool      `json:"mergeable"`
	User      struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
	Head struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
}

// status is the console state of a pull request: merged, closed (rejected) or open.
func (p dictPull) status() string {
	switch {
	case p.MergedAt != nil:
		return "merged"
	case p.State == "closed":
		return "closed"
	}
	return "open"
}

// dictionaryBranch reports whether p is a website submission pull request: a community-words/ branch in the dictionary repository itself, since a fork can use the same branch name.
func dictionaryBranch(p dictPull, repo string) bool {
	return strings.HasPrefix(p.Head.Ref, wordSubmissionBranches) && p.Head.Repo != nil && strings.EqualFold(p.Head.Repo.FullName, repo)
}

type dictPRCounts struct {
	Total   int `json:"total"`
	New     int `json:"new"`
	Dup     int `json:"dup"`
	Flagged int `json:"flagged"`
}

type dictPRSummary struct {
	Number    int           `json:"number"`
	Title     string        `json:"title"`
	Author    string        `json:"author"`
	AuthorBot bool          `json:"author_bot"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
	State     string        `json:"state"`
	URL       string        `json:"url"`
	Note      string        `json:"note"`
	Counts    *dictPRCounts `json:"counts"`
}

func summarizeDictPull(p dictPull) dictPRSummary {
	return dictPRSummary{Number: p.Number, Title: p.Title, Author: p.User.Login, AuthorBot: p.User.Type == "Bot" || strings.HasSuffix(p.User.Login, "[bot]"),
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt, State: p.status(), URL: p.HTMLURL}
}

// dictEntry is one line a pull request adds. Word and Pinyin are the line's first two columns: word and reading for words, typed word and display form for English words, source and gloss for translations.
type dictEntry struct {
	Index  int    `json:"index"`
	File   string `json:"file"`
	Kind   string `json:"kind"`
	Word   string `json:"word"`
	Pinyin string `json:"pinyin"`
	Flag   string `json:"flag"`
	Reason string `json:"reason,omitempty"`
}

// dictFile is one submission file of a pull request: its content at the base and head commits, the head blob SHA, and the head line numbers of the entries the pull request adds.
type dictFile struct {
	kind    submissionKind
	base    string
	head    string
	blobSHA string
	added   []int
}

// dictGitHubError answers a failed GitHub call made before any write: rejected App credentials are a configuration problem, anything else is GitHub being unavailable.
func dictGitHubError(w http.ResponseWriter, err error) {
	slog.Error("dictionary pull requests: GitHub call failed", "reason", err.Error())
	if errors.Is(err, githubapp.ErrRejected) {
		fail(w, 502, "github_rejected")
		return
	}
	fail(w, 503, "github_unavailable")
}

// dictGitHubStatus maps a GitHub HTTP error to the console's error envelope.
func dictGitHubStatus(w http.ResponseWriter, phase string, status int) {
	switch {
	case status == 404:
		fail(w, 404, "not_found")
	case status >= 500:
		slog.Error("dictionary pull requests: GitHub answered with an error", "phase", phase, "status", status)
		fail(w, 503, "github_unavailable")
	default:
		slog.Error("dictionary pull requests: GitHub answered with an error", "phase", phase, "status", status)
		fail(w, 502, "github_error")
	}
}

// adminDictPRs serves every path under /api/dict-prs: GET /api/dict-prs, GET /api/dict-prs/{n}, POST /api/dict-prs/{n}/approve, /reject and /trim.
func (s *Server) adminDictPRs(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/dict-prs"), "/")
	if rest == "" {
		if r.Method != "GET" {
			fail(w, 405, "method_not_allowed")
			return
		}
		if gh, ok := s.adminGitHubApp(w); ok {
			s.listDictPRs(w, r, gh)
		}
		return
	}
	// 路由最多接受编号和操作两段，第三段只用于拒绝多余路径。
	parts := strings.SplitN(rest, "/", 3)
	number, err := strconv.Atoi(parts[0])
	if err != nil || number <= 0 || number > 1<<30 || strconv.Itoa(number) != parts[0] {
		fail(w, 400, "invalid_id")
		return
	}
	switch {
	case len(parts) == 1:
		if r.Method != "GET" {
			fail(w, 405, "method_not_allowed")
			return
		}
		if gh, ok := s.adminGitHubApp(w); ok {
			s.dictPRDetail(w, r, gh, number)
		}
	case len(parts) == 2 && slices.Contains([]string{"approve", "reject", "trim"}, parts[1]):
		if r.Method != "POST" {
			fail(w, 405, "method_not_allowed")
			return
		}
		gh, ok := s.adminGitHubApp(w)
		if !ok || !requirePerm(w, r, account.PermReviewDictPR) {
			return
		}
		switch parts[1] {
		case "approve":
			s.approveDictPR(w, r, gh, number)
		case "reject":
			s.rejectDictPR(w, r, gh, number)
		default:
			s.trimDictPR(w, r, gh, number)
		}
	default:
		fail(w, 404, "not_found")
	}
}

func (s *Server) dictRepo() string { return s.config.Admin.GitHub.DictionaryRepo }

// fetchDictPulls reads the most recent dictionary pull requests (cached by githubapp for a minute), refreshes the search index and announces open pull requests not announced before. A GitHub HTTP error is returned as its status with a nil error.
func (s *Server) fetchDictPulls(ctx context.Context, gh *githubapp.Client) ([]dictPull, int, error) {
	repo := s.dictRepo()
	token, err := gh.Token(ctx, repo, dictPRPermissions)
	if err != nil {
		return nil, 0, err
	}
	res, err := gh.Get(ctx, token, "/repos/"+repo+"/pulls?"+url.Values{"state": {"all"}, "sort": {"created"}, "direction": {"desc"}, "per_page": {strconv.Itoa(dictPRListSize)}}.Encode())
	if err != nil {
		return nil, 0, errors.Join(githubapp.ErrUnavailable, err)
	}
	if !res.OK() {
		return nil, res.Status, nil
	}
	var all []dictPull
	if err = res.Decode(&all); err != nil {
		return nil, 0, errors.Join(githubapp.ErrUnavailable, errors.New("list pull requests: invalid response"))
	}
	pulls := slices.DeleteFunc(all, func(p dictPull) bool { return !dictionaryBranch(p, repo) })
	st := s.dictPRState()
	index := make([]dictPRSummary, len(pulls))
	var fresh []dictPull
	st.mu.Lock()
	for i, p := range pulls {
		index[i] = summarizeDictPull(p)
		index[i].Note = st.notes[p.Number]
		// Claimed under the lock, so concurrent list reads (the page and the shell poll) announce a pull request once.
		if p.status() == "open" && !st.notified[p.Number] {
			st.notified[p.Number] = true
			fresh = append(fresh, p)
		}
	}
	st.index = index
	st.mu.Unlock()
	for _, p := range fresh {
		// NotifyOnce checks the database, so a restart or another replica, which start with an empty notified map, never announces the same pull request again.
		err := s.accounts.NotifyOnce(ctx, account.Notification{Kind: account.NotifyDictPR, Title: "词库 PR #" + strconv.Itoa(p.Number) + " 等待审核", TargetPage: "dictpr", TargetID: strconv.Itoa(p.Number)})
		if err != nil {
			slog.Warn("dictionary pull requests: notification not recorded", "pull", p.Number, "reason", err.Error())
			// Released so a later read tries again.
			st.mu.Lock()
			delete(st.notified, p.Number)
			st.mu.Unlock()
		}
	}
	return pulls, 200, nil
}

// submissionNotes returns the latest non-empty submitter note of each pull request, and the submissions themselves; notes are optional context, so a failure only logs.
func (s *Server) submissionNotes(ctx context.Context, numbers []int) (map[int]string, []account.WordSubmission) {
	notes := map[int]string{}
	if len(numbers) == 0 {
		return notes, nil
	}
	subs, err := s.accounts.WordSubmissions(ctx, numbers)
	if err != nil {
		slog.Warn("dictionary pull requests: submission notes unavailable", "reason", err.Error())
		return notes, nil
	}
	latest := map[int]time.Time{}
	for _, sub := range subs {
		if sub.Note != "" && !sub.Created.Before(latest[sub.PRNumber]) {
			notes[sub.PRNumber], latest[sub.PRNumber] = sub.Note, sub.Created
		}
	}
	// The global search matches notes too, so it remembers the latest ones it was given.
	st := s.dictPRState()
	st.mu.Lock()
	for n, note := range notes {
		st.notes[n] = note
	}
	for i := range st.index {
		if note, ok := notes[st.index[i].Number]; ok {
			st.index[i].Note = note
		}
	}
	st.mu.Unlock()
	return notes, subs
}

func (s *Server) listDictPRs(w http.ResponseWriter, r *http.Request, gh *githubapp.Client) {
	state := r.URL.Query().Get("state")
	if !slices.Contains([]string{"", "all", "open", "merged", "closed"}, state) {
		fail(w, 400, "invalid_state")
		return
	}
	pulls, status, err := s.fetchDictPulls(r.Context(), gh)
	if err != nil {
		dictGitHubError(w, err)
		return
	}
	if status != 200 {
		dictGitHubStatus(w, "list pull requests", status)
		return
	}
	counts := map[string]int{"open": 0, "merged": 0, "closed": 0, "all": len(pulls)}
	numbers := make([]int, len(pulls))
	for i, p := range pulls {
		counts[p.status()]++
		numbers[i] = p.Number
	}
	notes, _ := s.submissionNotes(r.Context(), numbers)
	items := []dictPRSummary{}
	enriched := 0
	for _, p := range pulls {
		if state != "" && state != "all" && p.status() != state {
			continue
		}
		item := summarizeDictPull(p)
		item.Note = notes[p.Number]
		if item.State == "open" && enriched < dictPRCountedOpen {
			enriched++
			if files, err := s.readDictFiles(r.Context(), gh, p); err != nil {
				slog.Warn("dictionary pull requests: entry counts unavailable", "pull", p.Number, "reason", err.Error())
			} else {
				c := countDictEntries(s.checkDictEntries(r.Context(), files))
				item.Counts = &c
			}
		}
		items = append(items, item)
	}
	respond(w, 200, map[string]any{"repo": s.dictRepo(), "items": items, "counts": counts})
}

// readDictPull reads one pull request bypassing the cache, so writes act on its current head, and answers 404 for a pull request that is not a dictionary submission. A GitHub HTTP error is returned as its status with a nil error.
func (s *Server) readDictPull(ctx context.Context, gh *githubapp.Client, token string, number int) (dictPull, int, error) {
	var p dictPull
	res, err := gh.Do(ctx, token, "GET", "/repos/"+s.dictRepo()+"/pulls/"+strconv.Itoa(number), nil)
	if err != nil {
		return p, 0, errors.Join(githubapp.ErrUnavailable, err)
	}
	if !res.OK() {
		return p, res.Status, nil
	}
	if res.Decode(&p) != nil || p.Head.SHA == "" || p.Base.SHA == "" {
		return p, 0, errors.Join(githubapp.ErrUnavailable, errors.New("read pull request: invalid response"))
	}
	if !dictionaryBranch(p, s.dictRepo()) {
		return p, 404, nil
	}
	return p, 200, nil
}

// readDictFile reads path at an immutable commit SHA through the read cache and returns its text and blob SHA; a file the commit does not have reads as empty.
func (s *Server) readDictFile(ctx context.Context, gh *githubapp.Client, token, path, ref string) (string, string, error) {
	res, err := gh.Get(ctx, token, "/repos/"+s.dictRepo()+"/contents/"+path+"?"+url.Values{"ref": {ref}}.Encode())
	switch {
	case err != nil:
		return "", "", errors.Join(githubapp.ErrUnavailable, err)
	case res.Status == 404:
		return "", "", nil
	case !res.OK():
		return "", "", errors.Join(githubapp.ErrUnavailable, errors.New("read "+path+": status "+strconv.Itoa(res.Status)))
	}
	var file githubFile
	if res.Decode(&file) != nil {
		return "", "", errors.Join(githubapp.ErrUnavailable, errors.New("read "+path+": invalid response"))
	}
	content, ok := file.text()
	if !ok {
		return "", "", errors.Join(githubapp.ErrUnavailable, errors.New("read "+path+": unexpected content encoding"))
	}
	return content, file.SHA, nil
}

// readDictFiles reads every submission file at the pull request's base and head commits and locates the lines the head adds.
func (s *Server) readDictFiles(ctx context.Context, gh *githubapp.Client, p dictPull) ([]dictFile, error) {
	token, err := gh.Token(ctx, s.dictRepo(), dictPRPermissions)
	if err != nil {
		return nil, err
	}
	files := make([]dictFile, 0, len(submissionKinds))
	for _, kind := range submissionKinds {
		base, _, err := s.readDictFile(ctx, gh, token, kind.file, p.Base.SHA)
		if err != nil {
			return nil, err
		}
		head, blob, err := s.readDictFile(ctx, gh, token, kind.file, p.Head.SHA)
		if err != nil {
			return nil, err
		}
		files = append(files, dictFile{kind: kind, base: base, head: head, blobSHA: blob, added: addedLinePositions(base, head)})
	}
	return files, nil
}

// addedLinePositions returns the indexes (into head split on newlines) of the entry lines head adds over base, counted the way addedLines counts them: stripped, blank and # lines skipped, each base line cancelling one equal head line in order.
func addedLinePositions(base, head string) []int {
	if head == "" || base == head {
		return nil
	}
	remaining := map[string]int{}
	for line := range strings.SplitSeq(base, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			remaining[line]++
		}
	}
	var added []int
	i := 0
	for line := range strings.SplitSeq(head, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case remaining[line] > 0:
			remaining[line]--
		default:
			added = append(added, i)
		}
		i++
	}
	return added
}

// dictLineColumns splits an entry line into its first two columns as the dictionary build reads them.
func dictLineColumns(line string) (string, string) {
	first, rest, _ := strings.Cut(strings.TrimSpace(line), "\t")
	second, _, _ := strings.Cut(rest, "\t")
	return strings.TrimSpace(first), strings.TrimSpace(second)
}

// checkDictEntries numbers the added entries across the files in title order (words, English words, translations) and flags each one: a sensitive-word hit, an entry the website form would reject, or a duplicate of the base file, the shipped dictionary or an earlier entry of the same pull request.
func (s *Server) checkDictEntries(ctx context.Context, files []dictFile) []dictEntry {
	entries := []dictEntry{}
	for _, f := range files {
		if len(f.added) == 0 {
			continue
		}
		first := len(entries)
		keys := make([]submissionLine, 0, len(f.added))
		// 新增位置由 addedLinePositions 按物理行递增生成，读完最后一条即可停止。
		at, next := 0, 0
		for line := range strings.SplitSeq(f.head, "\n") {
			if at == f.added[next] {
				word, second := dictLineColumns(line)
				entries = append(entries, dictEntry{Index: len(entries), File: f.kind.file, Kind: f.kind.name, Word: word, Pinyin: second, Flag: dictFlagNew})
				keys = append(keys, submissionLine{key: word + "\t" + second})
				next++
				if next == len(f.added) {
					break
				}
			}
			at++
		}
		s.flagDictFile(ctx, f, entries[first:], keys)
	}
	// Reviewing is a read, repeated on every list and detail view; only the website submission screening counts hits.
	matcher := s.accounts.SensitivePreview()
	for i := range entries {
		e := &entries[i]
		text := e.Word
		if e.Kind != kindWords.name {
			text = e.Word + " " + e.Pinyin
		}
		hits, err := matcher.Match(ctx, text)
		if err != nil {
			slog.Warn("dictionary pull requests: sensitive word check skipped", "reason", err.Error())
			break
		}
		if len(hits) > 0 {
			e.Flag, e.Reason = dictFlagAd, "命中敏感词「"+hits[0].Pattern+"」"
		}
	}
	return entries
}

// flagDictFile marks the invalid and duplicate entries of one file; entries is the file's part of the pull request's entries and keys their first two columns.
func (s *Server) flagDictFile(ctx context.Context, f dictFile, entries []dictEntry, keys []submissionLine) {
	mark := func(i int, flag, reason string) {
		if entries[i].Flag == dictFlagNew || (flag == dictFlagBad && entries[i].Flag == dictFlagDup) {
			entries[i].Flag, entries[i].Reason = flag, reason
		}
	}
	seen := map[string]bool{}
	for i, k := range keys {
		if seen[k.key] {
			mark(i, dictFlagDup, "本 PR 中重复")
		}
		seen[k.key] = true
	}
	for _, i := range listedLines(f.base, keys) {
		mark(i, dictFlagDup, f.kind.listed)
	}
	var valid []int
	for i, e := range entries {
		var rejected []wordRejection
		switch f.kind {
		case kindWords:
			rejected = validateWordEntries([]wordSubmissionEntry{{Word: e.Word, Pinyin: e.Pinyin}})
		case kindEnglish:
			rejected = validateEnglishEntries([]englishSubmissionEntry{{Word: e.Word, Display: e.Pinyin}})
		default:
			rejected = validateTranslationEntries([]translationSubmissionEntry{{Source: e.Word, Gloss: e.Pinyin}})
		}
		if len(rejected) > 0 {
			mark(i, dictFlagBad, rejected[0].Reason)
			continue
		}
		valid = append(valid, i)
	}
	// Translations are not checked against the Engine: overriding what it ships is their purpose.
	if f.kind == kindTranslations {
		return
	}
	for start := 0; start < len(valid); start += dictPREngineBatch {
		chunk := valid[start:min(start+dictPREngineBatch, len(valid))]
		var err error
		if f.kind == kindWords {
			batch := make([]wordSubmissionEntry, len(chunk))
			for j, i := range chunk {
				batch[j] = wordSubmissionEntry{Word: entries[i].Word, Pinyin: entries[i].Pinyin}
			}
			err = s.shippedWords(ctx, batch)
		} else {
			batch := make([]englishSubmissionEntry, len(chunk))
			for j, i := range chunk {
				batch[j] = englishSubmissionEntry{Word: entries[i].Word, Display: entries[i].Pinyin}
			}
			err = s.shippedEnglish(ctx, batch)
		}
		var listed alreadyListedError
		if errors.As(err, &listed) {
			for _, j := range listed {
				mark(chunk[j], dictFlagDup, f.kind.listed)
			}
		}
	}
}

func countDictEntries(entries []dictEntry) dictPRCounts {
	c := dictPRCounts{Total: len(entries)}
	for _, e := range entries {
		switch e.Flag {
		case dictFlagNew:
			c.New++
		case dictFlagDup:
			c.Dup++
		default:
			c.Flagged++
		}
	}
	return c
}

func (s *Server) dictPRDetail(w http.ResponseWriter, r *http.Request, gh *githubapp.Client, number int) {
	ctx := r.Context()
	token, err := gh.Token(ctx, s.dictRepo(), dictPRPermissions)
	if err != nil {
		dictGitHubError(w, err)
		return
	}
	p, status, err := s.readDictPull(ctx, gh, token, number)
	if err != nil {
		dictGitHubError(w, err)
		return
	}
	if status != 200 {
		dictGitHubStatus(w, "read pull request", status)
		return
	}
	files, err := s.readDictFiles(ctx, gh, p)
	if err != nil {
		dictGitHubError(w, err)
		return
	}
	entries := s.checkDictEntries(ctx, files)
	notes, subs := s.submissionNotes(ctx, []int{number})
	submissions := []map[string]any{}
	for _, sub := range subs {
		if sub.PRNumber == number {
			submissions = append(submissions, map[string]any{"kind": sub.Kind, "note": sub.Note, "created_at": sub.Created})
		}
	}
	summary := summarizeDictPull(p)
	summary.Note = notes[number]
	counts := countDictEntries(entries)
	summary.Counts = &counts
	respond(w, 200, map[string]any{"repo": s.dictRepo(), "pull": summary, "head_sha": p.Head.SHA, "mergeable": p.Mergeable, "entries": entries, "submissions": submissions})
}

// dictKeepRequest is the body of approve and trim. Keep lists the entry indexes to keep (approve without it keeps every entry); HeadSHA is the head the reviewer saw, required with Keep and optional for an approval of every entry, and a pull request that moved since answers 409 pr_changed so indexes never apply to entries the reviewer has not seen.
type dictKeepRequest struct {
	Keep    *[]int `json:"keep"`
	HeadSHA string `json:"head_sha"`
}

// keptEntries resolves the keep list against the pull request's total entries: each index in range, none repeated, at least one. A nil list keeps every entry unless one is required.
func keptEntries(w http.ResponseWriter, keep *[]int, total int, required bool) (map[int]bool, bool) {
	set := map[int]bool{}
	if keep == nil {
		if required || total == 0 {
			fail(w, 400, "invalid_keep")
			return nil, false
		}
		for i := range total {
			set[i] = true
		}
		return set, true
	}
	if len(*keep) == 0 || len(*keep) > dictPRMaxKeep {
		fail(w, 400, "invalid_keep")
		return nil, false
	}
	for _, i := range *keep {
		if i < 0 || i >= total || set[i] {
			fail(w, 400, "invalid_keep")
			return nil, false
		}
		set[i] = true
	}
	return set, true
}

// openDictPull reads the pull request a write acts on and answers the failure itself: not a dictionary pull request, not open any more, or moved past the head the reviewer saw.
func (s *Server) openDictPull(w http.ResponseWriter, ctx context.Context, gh *githubapp.Client, number int, headSHA string) (string, dictPull, bool) {
	token, err := gh.Token(ctx, s.dictRepo(), dictPRPermissions)
	if err != nil {
		dictGitHubError(w, err)
		return "", dictPull{}, false
	}
	p, status, err := s.readDictPull(ctx, gh, token, number)
	switch {
	case err != nil:
		dictGitHubError(w, err)
	case status != 200:
		dictGitHubStatus(w, "read pull request", status)
	case p.status() != "open":
		fail(w, 409, "not_open")
	case headSHA != "" && headSHA != p.Head.SHA:
		fail(w, 409, "pr_changed")
	default:
		return token, p, true
	}
	return "", p, false
}

// trimFiles rewrites the pull request branch so that only the kept entries remain, one commit per changed file, each a compare-and-swap on the file's blob SHA as in wordSubmitter.submit and checked to sit directly on the previous head, then retitles the pull request after what it now adds. It returns the new head SHA, the title and the number of entries removed; after a partial failure the removed count still covers the files already rewritten.
func (s *Server) trimFiles(ctx context.Context, gh *githubapp.Client, token string, p dictPull, files []dictFile, keep map[int]bool) (string, string, int, error) {
	repo := "/repos/" + s.dictRepo()
	head, removed, index := p.Head.SHA, 0, 0
	counts := map[string]int{}
	for _, f := range files {
		drop := map[int]bool{}
		for _, at := range f.added {
			if keep[index] {
				counts[f.kind.name]++
			} else {
				drop[at] = true
			}
			index++
		}
		if len(drop) == 0 {
			continue
		}
		content := trimDictionaryContent(f.head, drop)
		res, err := gh.Do(ctx, token, "PUT", repo+"/contents/"+f.kind.file, map[string]string{
			"message": "chore(custom): drop " + strconv.Itoa(len(drop)) + " entries during review",
			"content": base64.StdEncoding.EncodeToString([]byte(content)),
			"sha":     f.blobSHA,
			"branch":  p.Head.Ref,
		})
		switch {
		case err != nil || res.Status >= 500:
			return head, p.Title, removed, phaseError(errWordsUncertain, "commit "+f.kind.file, githubResponse{status: res.Status}, err)
		case res.Status == 409 || res.Status == 422:
			return head, p.Title, removed, errDictPRConflict
		case !res.OK():
			return head, p.Title, removed, phaseError(errWordsUnavailable, "commit "+f.kind.file, githubResponse{status: res.Status}, nil)
		}
		var commit struct {
			Commit struct {
				SHA     string `json:"sha"`
				Parents []struct {
					SHA string `json:"sha"`
				} `json:"parents"`
			} `json:"commit"`
		}
		removed += len(drop)
		if res.Decode(&commit) != nil || commit.Commit.SHA == "" {
			return head, p.Title, removed, phaseError(errWordsUncertain, "commit "+f.kind.file, githubResponse{status: res.Status}, errors.New("missing commit SHA"))
		}
		// The blob SHA only guards this one file, and GitHub stacks the commit on whatever the branch tip is now. A commit whose parent is not the head this trim started from means something else landed on the branch (a website submission to another file), so the branch now holds entries nobody reviewed: stop before any further write or merge.
		if len(commit.Commit.Parents) != 1 || commit.Commit.Parents[0].SHA != head {
			slog.Warn("dictionary pull requests: branch moved during trim", "pull", p.Number, "expected_parent", head, "commit", commit.Commit.SHA)
			return commit.Commit.SHA, p.Title, removed, errDictPRConflict
		}
		head = commit.Commit.SHA
	}
	// release-please reads the squash commit subject, so the title keeps describing what the branch adds. The entries are committed by now, so a failure only logs and the old title stays.
	title := submissionTitle(counts)
	if title != p.Title {
		res, err := gh.Do(ctx, token, "PATCH", repo+"/pulls/"+strconv.Itoa(p.Number), map[string]string{"title": title})
		if err != nil || !res.OK() {
			slog.Warn("dictionary pull requests: title not updated after trim", "pull", p.Number, "status", res.Status)
			title = p.Title
		}
	}
	return head, title, removed, nil
}

// trimDictionaryContent 删除指定物理行并保留其余行之间的原始换行语义，不构造整份行切片。
func trimDictionaryContent(content string, drop map[int]bool) string {
	// 只保存少量连续保留片段；超出固定容量时仍按行重建，不为整份词库建立行索引。
	var chunks [16]string
	chunkCount, start, end, offset := 0, -1, 0, 0
	resumeOffset, resumeLine := 0, 0
	saveChunk := func() {
		if chunkCount < len(chunks) {
			chunks[chunkCount] = content[start:end]
		}
		chunkCount++
		start = -1
	}
	// 容量只计保留行及它们之间的分隔符，避免空结果仍持有整份输入大小的缓冲区。
	outputBytes, keptLines, line := 0, 0, 0
	for part := range strings.SplitSeq(content, "\n") {
		if !drop[line] {
			outputBytes += len(part)
			keptLines++
			if start == -1 {
				start = offset
				if chunkCount == len(chunks) {
					resumeOffset, resumeLine = offset, line
				}
			}
			end = offset + len(part)
		} else if start != -1 {
			saveChunk()
		}
		line++
		offset += len(part) + 1
	}
	if start != -1 {
		saveChunk()
	}
	if keptLines > 0 {
		outputBytes += keptLines - 1
	}
	if outputBytes == 0 {
		return ""
	}
	if outputBytes == len(content) {
		return content
	}
	var kept strings.Builder
	kept.Grow(outputBytes)
	for i, chunk := range chunks[:min(chunkCount, len(chunks))] {
		if i > 0 {
			kept.WriteByte('\n')
		}
		kept.WriteString(chunk)
	}
	if chunkCount <= len(chunks) {
		return kept.String()
	}
	// 片段过多时从第一个未缓存片段继续，避免重新扫描已复制的长前缀。
	line = resumeLine
	for part := range strings.SplitSeq(content[resumeOffset:], "\n") {
		if !drop[line] {
			kept.WriteByte('\n')
			kept.WriteString(part)
		}
		line++
	}
	return kept.String()
}

// trimFailure answers a failed trim: a concurrent change is a 409 the reviewer resolves by reloading, an unknown write outcome a 502 that must not be retried blindly.
func trimFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errDictPRConflict):
		fail(w, 409, "pr_changed")
	case errors.Is(err, errWordsUncertain):
		slog.Error("dictionary pull requests: trim outcome unknown", "reason", err.Error())
		fail(w, 502, "github_uncertain")
	default:
		slog.Error("dictionary pull requests: trim rejected", "reason", err.Error())
		fail(w, 502, "github_error")
	}
}

// auditDictPR 在 GitHub 已接受审核写入后保留操作者，并在请求取消后继续限时记录审计；审计失败只记日志，避免管理员重复执行已完成的审核。
func (s *Server) auditDictPR(ctx context.Context, action string, number int, detail map[string]any) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.accounts.Audit(ctx, action, strconv.Itoa(number), detail); err != nil {
		slog.Error("dictionary pull requests: audit not recorded", "action", action, "pull", number, "reason", err.Error())
	}
}

// onlySubmissionFiles makes sure the pull request changes nothing but the submission files, since review only shows their entries while a merge takes every file on the branch. It answers 409 unexpected_files (or the GitHub failure) itself and returns false. The merge is a compare-and-swap on the head read just before, so a file pushed after this check cannot slip into the merge.
func (s *Server) onlySubmissionFiles(w http.ResponseWriter, ctx context.Context, gh *githubapp.Client, token string, p dictPull) bool {
	res, err := gh.Do(ctx, token, "GET", "/repos/"+s.dictRepo()+"/pulls/"+strconv.Itoa(p.Number)+"/files?per_page=100", nil)
	if err != nil {
		dictGitHubError(w, errors.Join(githubapp.ErrUnavailable, err))
		return false
	}
	if !res.OK() {
		dictGitHubStatus(w, "list pull request files", res.Status)
		return false
	}
	var changed []struct {
		Filename         string `json:"filename"`
		PreviousFilename string `json:"previous_filename"`
	}
	if res.Decode(&changed) != nil {
		dictGitHubError(w, errors.Join(githubapp.ErrUnavailable, errors.New("list pull request files: invalid response")))
		return false
	}
	allowed := map[string]bool{}
	for _, kind := range submissionKinds {
		allowed[kind.file] = true
	}
	for _, f := range changed {
		if !allowed[f.Filename] || (f.PreviousFilename != "" && !allowed[f.PreviousFilename]) {
			slog.Warn("dictionary pull requests: review refused, the branch changes other files", "pull", p.Number, "file", f.Filename)
			fail(w, 409, "unexpected_files")
			return false
		}
	}
	// A page can hold at most one entry per submission file; more means GitHub listed files beyond them.
	if len(changed) > len(submissionKinds) {
		fail(w, 409, "unexpected_files")
		return false
	}
	return true
}

// prepareKeep reads the open pull request and its files and resolves the keep list; it answers every failure itself. Entry indexes only mean something for the head they were read from, so a keep list must come with that head.
func (s *Server) prepareKeep(w http.ResponseWriter, ctx context.Context, gh *githubapp.Client, number int, body dictKeepRequest, required bool) (string, dictPull, []dictFile, map[int]bool, int, bool) {
	if body.Keep != nil && (body.HeadSHA == "" || len(body.HeadSHA) > 64) {
		fail(w, 400, "invalid_head_sha")
		return "", dictPull{}, nil, nil, 0, false
	}
	token, p, ok := s.openDictPull(w, ctx, gh, number, body.HeadSHA)
	if !ok {
		return "", p, nil, nil, 0, false
	}
	if !s.onlySubmissionFiles(w, ctx, gh, token, p) {
		return "", p, nil, nil, 0, false
	}
	files, err := s.readDictFiles(ctx, gh, p)
	if err != nil {
		dictGitHubError(w, err)
		return "", p, nil, nil, 0, false
	}
	total := 0
	for _, f := range files {
		total += len(f.added)
	}
	keep, ok := keptEntries(w, body.Keep, total, required)
	return token, p, files, keep, total, ok
}

// lockDictPRWrite 与官网投稿共用仓库锁，从读取 PR 起保护整个审核，避免多副本关闭仍在写入的投稿分支。
func (s *Server) lockDictPRWrite(w http.ResponseWriter, ctx context.Context) (func(), bool) {
	return s.lockAdminWrite(w, ctx, "word-submissions:"+strings.ToLower(s.dictRepo()))
}

func (s *Server) trimDictPR(w http.ResponseWriter, r *http.Request, gh *githubapp.Client, number int) {
	var body dictKeepRequest
	if !decode(w, r, &body) {
		return
	}
	ctx := r.Context()
	unlock, ok := s.lockDictPRWrite(w, ctx)
	if !ok {
		return
	}
	defer unlock()
	token, p, files, keep, _, ok := s.prepareKeep(w, ctx, gh, number, body, true)
	if !ok {
		return
	}
	head, _, removed, err := s.trimFiles(ctx, gh, token, p, files, keep)
	gh.Invalidate("/repos/" + s.dictRepo() + "/pulls")
	unlock()
	if removed > 0 {
		s.auditDictPR(ctx, "dict_pr_trim", number, map[string]any{"count": len(keep), "removed": removed})
	}
	if err != nil {
		trimFailure(w, err)
		return
	}
	respond(w, 200, map[string]any{"ok": true, "count": len(keep), "removed": removed, "head_sha": head})
}

func (s *Server) approveDictPR(w http.ResponseWriter, r *http.Request, gh *githubapp.Client, number int) {
	var body dictKeepRequest
	if !decode(w, r, &body) {
		return
	}
	ctx := r.Context()
	unlock, ok := s.lockDictPRWrite(w, ctx)
	if !ok {
		return
	}
	defer unlock()
	token, p, files, keep, total, ok := s.prepareKeep(w, ctx, gh, number, body, false)
	if !ok {
		return
	}
	repo := "/repos/" + s.dictRepo()
	head, title, removed := p.Head.SHA, p.Title, 0
	// 合并结果确定或停止精简后先归还锁连接，再记录已完成的精简，避免审计等待同一连接池。
	unlockAndAuditTrim := func() {
		unlock()
		if removed > 0 {
			s.auditDictPR(ctx, "dict_pr_trim", number, map[string]any{"count": len(keep), "removed": removed})
		}
	}
	if len(keep) < total {
		var err error
		head, title, removed, err = s.trimFiles(ctx, gh, token, p, files, keep)
		if err != nil {
			gh.Invalidate(repo + "/pulls")
			unlockAndAuditTrim()
			trimFailure(w, err)
			return
		}
	}
	// The head SHA makes the merge a compare-and-swap too: trimFiles refuses a branch that moved under its commits, and the merge refuses one that moved after them, so a submission that lands meanwhile is never merged unreviewed.
	res, err := gh.Do(ctx, token, "PUT", repo+"/pulls/"+strconv.Itoa(number)+"/merge", map[string]string{"merge_method": "squash", "sha": head, "commit_title": title + " (#" + strconv.Itoa(number) + ")"})
	gh.Invalidate(repo + "/pulls")
	unlockAndAuditTrim()
	switch {
	case err != nil || res.Status >= 500:
		slog.Error("dictionary pull requests: merge outcome unknown", "pull", number, "status", res.Status)
		fail(w, 502, "github_uncertain")
		return
	case res.Status == 409:
		fail(w, 409, "pr_changed")
		return
	case res.Status == 405 || res.Status == 422:
		fail(w, 409, "not_mergeable")
		return
	case !res.OK():
		dictGitHubStatus(w, "merge", res.Status)
		return
	}
	var merged struct {
		Merged bool   `json:"merged"`
		SHA    string `json:"sha"`
	}
	if res.Decode(&merged) != nil || !merged.Merged || merged.SHA == "" {
		slog.Error("dictionary pull requests: merge confirmation missing", "pull", number, "status", res.Status)
		fail(w, 502, "github_uncertain")
		return
	}
	s.auditDictPR(ctx, "dict_pr_approve", number, map[string]any{"count": len(keep), "removed": removed})
	respond(w, 200, map[string]any{"ok": true, "merged": true, "count": len(keep), "removed": removed})
}

func (s *Server) rejectDictPR(w http.ResponseWriter, r *http.Request, gh *githubapp.Client, number int) {
	// HeadSHA is the head the reviewer saw: website submissions keep landing on the rolling branch, so a pull request that moved since answers 409 pr_changed instead of rejecting entries nobody reviewed.
	var body struct {
		Reason  string `json:"reason"`
		HeadSHA string `json:"head_sha"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.HeadSHA == "" || len(body.HeadSHA) > 64 {
		fail(w, 400, "invalid_head_sha")
		return
	}
	reason := strings.TrimSpace(body.Reason)
	if reason == "" || !utf8.ValidString(reason) || utf8.RuneCountInString(reason) > dictPRReasonChars || strings.ContainsFunc(reason, func(c rune) bool { return (c < 0x20 && c != '\n' && c != '\t') || c == 0x7f }) {
		fail(w, 400, "invalid_reason")
		return
	}
	ctx := r.Context()
	unlock, ok := s.lockDictPRWrite(w, ctx)
	if !ok {
		return
	}
	defer unlock()
	token, _, ok := s.openDictPull(w, ctx, gh, number, body.HeadSHA)
	if !ok {
		return
	}
	repo := "/repos/" + s.dictRepo()
	res, err := gh.Do(ctx, token, "POST", repo+"/issues/"+strconv.Itoa(number)+"/comments", map[string]string{"body": "审核未通过：" + reason})
	if err != nil || !res.OK() {
		slog.Error("dictionary pull requests: rejection comment failed", "pull", number, "status", res.Status)
		fail(w, 502, "github_error")
		return
	}
	// 评论已经确认写入后，即使管理员断开连接也继续限时关闭拉取请求，避免重试产生重复评论并留下开放的拒绝请求。
	closeCtx, cancelClose := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelClose()
	res, err = gh.Do(closeCtx, token, "PATCH", repo+"/pulls/"+strconv.Itoa(number), map[string]string{"state": "closed"})
	gh.Invalidate(repo + "/pulls")
	switch {
	case err != nil || res.Status >= 500:
		slog.Error("dictionary pull requests: close outcome unknown", "pull", number, "status", res.Status)
		fail(w, 502, "github_uncertain")
		return
	case !res.OK():
		dictGitHubStatus(w, "close", res.Status)
		return
	}
	unlock()
	s.auditDictPR(closeCtx, "dict_pr_reject", number, map[string]any{"reason": reason})
	respond(w, 200, map[string]any{"ok": true})
}

// searchDictPRs matches q against the cached dictionary pull requests (number, title and submitter note) for the global search; it never calls GitHub.
func (s *Server) searchDictPRs(q string) []account.AdminSearchHit {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return nil
	}
	st := s.dictPRState()
	st.mu.Lock()
	defer st.mu.Unlock()
	var hits []account.AdminSearchHit
	for _, p := range st.index {
		title := "#" + strconv.Itoa(p.Number) + " " + p.Title
		if !strings.Contains(strings.ToLower(title), q) && !strings.Contains(strings.ToLower(p.Note), q) {
			continue
		}
		// Website pull request titles only count entries, so the submitter's note names the pull request when there is one.
		if p.Note != "" {
			title = "#" + strconv.Itoa(p.Number) + " 词库：" + p.Note
		}
		hits = append(hits, account.AdminSearchHit{Kind: "dict_pr", ID: strconv.Itoa(p.Number), Title: title, Where: "词库审核", Target: "dictpr"})
	}
	return hits
}

// pendingDictPRs counts open dictionary pull requests for the shell badge, through the same cached read as the list.
func (s *Server) pendingDictPRs(ctx context.Context) (int, error) {
	if s.adminGitHub == nil {
		return 0, nil
	}
	pulls, status, err := s.fetchDictPulls(ctx, s.adminGitHub)
	if err != nil {
		return 0, err
	}
	if status != 200 {
		return 0, errors.New("list dictionary pull requests: status " + strconv.Itoa(status))
	}
	open := 0
	for _, p := range pulls {
		if p.status() == "open" {
			open++
		}
	}
	return open, nil
}
