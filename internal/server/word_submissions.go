package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	_ "embed"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/metasequoiaime/MSIME-Backend/internal/engine"
	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

// Anonymous dictionary submissions from the website (msime-web#213). A visitor proposes words with their quanpin reading, English words with the form to show, or candidate-window translations; after Cloudflare Turnstile and a per-address PostgreSQL rate limit, the server appends them to custom/words.txt, custom/english.txt or custom/translations.txt in metasequoiaime/msime-dictionary on one rolling pull request that maintainers review. Nothing about the visitor is stored, and entries, notes and tokens are never logged.

const (
	wordSubmissionsPath      = "/v1/community/word-submissions"
	wordSubmissionBodyBytes  = 16 << 10
	wordSubmissionMaxEntries = 20
	wordSubmissionMaxChars   = 16
	wordSubmissionNoteChars  = 500
	wordSubmissionTokenBytes = 2048
	// Submitters do not choose weights. A word gets the median weight of the base dictionary entries with as many syllables; this one is used only while the Engine cannot compute the medians.
	wordSubmissionFallbackWeight = 5000
	// English words keep the lowest weight: they complete a typed prefix and must not push the dictionary's own words down.
	englishSubmissionWeight = 1
	englishWordMaxBytes     = 64
	englishDisplayMaxChars  = 64
	translationSourceChars  = 64
	translationGlossChars   = 200
	wordSubmissionBranches  = "community-words/"
	wordSubmissionTimeout   = 45 * time.Second
	// How long a submission waits for another replica's GitHub write to finish. One write takes a few seconds; the wait stays well inside wordSubmissionTimeout so the GitHub calls that follow still have time.
	wordSubmissionLockWait = 15 * time.Second
	defaultTurnstileURL    = "https://challenges.cloudflare.com/turnstile/v0/siteverify"
	defaultGitHubAPIURL    = githubapp.DefaultAPIURL
)

// Conservative per-address limits. They count requests that passed Turnstile, so an automated client cannot use up a visitor's quota without solving a challenge. The shorter window is checked first so a burst does not also consume the daily allowance.
var wordSubmissionLimits = []struct {
	scope      string
	limit      int
	window     time.Duration
	retryAfter string
}{
	{"word-submissions-10m", 3, 10 * time.Minute, "600"},
	{"word-submissions-day", 20, 24 * time.Hour, "3600"},
}

// The quanpin syllable table is a copy of msime platforms/windows/installer/assets/tables/pinyin.txt (402 syllables, ü written as v, lüe/nüe as lve/nve), the same list the website form validates against, so the page and the server agree on what a valid reading is.
//
//go:embed pinyin_syllables.txt
var pinyinSyllableTable string

var pinyinSyllables = func() map[string]bool {
	set := map[string]bool{}
	for _, line := range strings.Split(pinyinSyllableTable, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			set[line] = true
		}
	}
	return set
}()

type TurnstileConfig struct {
	SiteKey       string `json:"site_key"`
	SecretEnv     string `json:"secret_env"`
	SiteverifyURL string `json:"siteverify_url,omitempty"`
	secret        string
}

type WordsGitHubConfig struct {
	AppID          int64  `json:"app_id"`
	InstallationID int64  `json:"installation_id"`
	PrivateKeyEnv  string `json:"private_key_env"`
	Repository     string `json:"repository"`
	Branch         string `json:"branch"`
	APIURL         string `json:"api_url,omitempty"`
	key            *rsa.PrivateKey
}

// WordSubmissionsConfig enables the anonymous website word form. An empty turnstile.site_key keeps the feature off; once it is set every other field is required and the server refuses to start with a partial configuration.
type WordSubmissionsConfig struct {
	// ClientIPHeader 是顶层 `client_ip_header` 的旧写法，仍然接受；Config.Validate 把设置了的那个同时写入两处，让词条表单和账号接口对访客身份的判断一致。
	ClientIPHeader string            `json:"client_ip_header"`
	Turnstile      TurnstileConfig   `json:"turnstile"`
	GitHub         WordsGitHubConfig `json:"github"`
	// siteProxySecret 是顶层官网代理密钥，由 Config.Validate 写入，不是本节的配置项。
	siteProxySecret string
}

func (c WordSubmissionsConfig) enabled() bool { return c.Turnstile.SiteKey != "" }

var (
	githubRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[A-Za-z0-9._-]{1,100}$`)
	githubBranchPattern     = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,100}$`)
	headerNamePattern       = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
)

func (c *WordSubmissionsConfig) validate(authEnabled bool, origins []string) error {
	if !c.enabled() {
		return nil
	}
	if !authEnabled {
		return errors.New("word_submissions requires auth.enabled: the per-address rate limit is stored in PostgreSQL")
	}
	if len(origins) == 0 {
		return errors.New("word_submissions requires allowed_origins: the form is only accepted from the website")
	}
	if c.ClientIPHeader != "" && !headerNamePattern.MatchString(c.ClientIPHeader) {
		return errors.New("word_submissions client_ip_header must be a header name")
	}
	t := &c.Turnstile
	if !validProviderCredential(t.SiteKey) || len(t.SiteKey) > 256 {
		return errors.New("word_submissions turnstile site_key is invalid")
	}
	t.secret = os.Getenv(t.SecretEnv)
	if t.SecretEnv == "" || !validProviderCredential(t.secret) {
		return errors.New("word_submissions turnstile secret_env missing or invalid")
	}
	if t.SiteverifyURL == "" {
		t.SiteverifyURL = defaultTurnstileURL
	}
	if !plainHTTPSURL(t.SiteverifyURL, false) {
		return errors.New("word_submissions turnstile siteverify_url must be an HTTPS URL without query or credentials")
	}
	g := &c.GitHub
	if g.AppID <= 0 || g.InstallationID <= 0 {
		return errors.New("word_submissions github app_id and installation_id are required")
	}
	if !githubRepositoryPattern.MatchString(g.Repository) {
		return errors.New("word_submissions github repository must be owner/name")
	}
	if g.Branch == "" {
		g.Branch = "main"
	}
	if !githubBranchPattern.MatchString(g.Branch) || strings.Contains(g.Branch, "..") || strings.HasPrefix(g.Branch, wordSubmissionBranches) {
		return errors.New("word_submissions github branch is invalid")
	}
	if g.APIURL == "" {
		g.APIURL = defaultGitHubAPIURL
	}
	g.APIURL = strings.TrimSuffix(g.APIURL, "/")
	if !plainHTTPSURL(g.APIURL, true) {
		return errors.New("word_submissions github api_url must be an HTTPS URL without query or credentials")
	}
	key, err := parseGitHubAppKey(os.Getenv(g.PrivateKeyEnv))
	if g.PrivateKeyEnv == "" || err != nil {
		return errors.New("word_submissions github private_key_env must hold the GitHub App RSA private key (PEM, PKCS#1 or PKCS#8)")
	}
	g.key = key
	return nil
}

func plainHTTPSURL(raw string, allowEmptyPath bool) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && (allowEmptyPath || u.Path != "")
}

// GitHub hands out PKCS#1 keys; converted keys are PKCS#8. Secret stores that cannot hold newlines often store the PEM with literal \n sequences, so those are accepted too.
func parseGitHubAppKey(raw string) (*rsa.PrivateKey, error) {
	if !strings.Contains(raw, "\n") {
		raw = strings.ReplaceAll(raw, `\n`, "\n")
	}
	block, _ := pem.Decode([]byte(raw))
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("GitHub App keys are RSA")
	}
	return key, nil
}

type rateLimiter interface {
	RateLimit(ctx context.Context, scope, subject string, limit int, window time.Duration) error
}

// sharedLocker is account.Service.WaitLock: a lock held across replicas until release, or account.ErrLockBusy after wait.
type sharedLocker interface {
	WaitLock(ctx context.Context, name string, wait time.Duration) (release func(), err error)
}

type wordSubmitter struct {
	config    WordSubmissionsConfig
	origins   []string
	hostnames []string
	limiter   rateLimiter
	client    *http.Client
	now       func() time.Time
	// Serialises the read-modify-write on GitHub within this process so two local requests never race for the same blob SHA. Without a database this is the only serialisation, which is correct for a single replica only.
	writes sync.Mutex
	// locks extends that serialisation to every replica through a PostgreSQL advisory lock keyed by the target repository; nil without a database.
	locks sharedLocker
	// Installation tokens minted for the dictionary repository, shared by every call through app().
	github githubapp.Cache
	// The base dictionary's median word weights, once the Engine has computed them.
	mediansMu     sync.Mutex
	medians       map[int]int
	mediansWarned bool
}

func newWordSubmitter(c WordSubmissionsConfig, origins []string, limiter rateLimiter) *wordSubmitter {
	ws := &wordSubmitter{config: c, origins: origins, limiter: limiter, now: time.Now,
		client: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	for _, o := range origins {
		if u, err := url.Parse(o); err == nil {
			ws.hostnames = append(ws.hostnames, u.Hostname())
		}
	}
	return ws
}

// Machine-readable code plus a message the website can show as is. The plain-string error (rather than the {code,message} object used elsewhere) is the contract the website form reads.
func wordsFail(w http.ResponseWriter, status int, code, message string) {
	respond(w, status, map[string]any{"error": message, "code": code})
}

func (s *Server) wordSubmissionSettings(w http.ResponseWriter, r *http.Request) {
	if s.words == nil {
		respond(w, 200, map[string]any{"enabled": false, "site_key": ""})
		return
	}
	respond(w, 200, map[string]any{"enabled": true, "site_key": s.words.config.Turnstile.SiteKey})
}

// submissionKind is one kind of website submission. Each appends to its own file in the dictionary repository; all kinds share one rolling branch and pull request.
type submissionKind struct {
	name     string // the request's kind
	file     string // the file entries are appended to
	singular string // how pull request titles count entries
	plural   string
	listed   string // the row reason for an entry the dictionary already has
}

var (
	kindWords        = submissionKind{"words", "custom/words.txt", "word", "words", "词库中已有这个词条"}
	kindEnglish      = submissionKind{"english", "custom/english.txt", "English word", "English words", "英文词库中已有这个词条"}
	kindTranslations = submissionKind{"translations", "custom/translations.txt", "translation", "translations", "翻译表中已有相同的翻译"}
	// Title order: words, English words, translations.
	submissionKinds = []submissionKind{kindWords, kindEnglish, kindTranslations}
)

type wordSubmissionEntry struct {
	Word   string `json:"word"`
	Pinyin string `json:"pinyin"`
}

// An English word: word is what the user types (the english_words key), display what the candidate shows.
type englishSubmissionEntry struct {
	Word    string `json:"word"`
	Display string `json:"display"`
}

// A candidate-window translation override: source is the word looked up, gloss what is shown for it.
type translationSubmissionEntry struct {
	Source string `json:"source"`
	Gloss  string `json:"gloss"`
}

type wordSubmissionRequest struct {
	// Kind is words (the default, so older clients keep working), english or translations; entries are decoded by it.
	Kind    string          `json:"kind"`
	Entries json.RawMessage `json:"entries"`
	Note    string          `json:"note"`
	Token   string          `json:"token"`
}

type wordRejection struct {
	Index  int    `json:"index"`
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

// submissionLine is one validated entry as it is written to its file.
type submissionLine struct {
	// The first two columns joined by a tab; an entry whose key its file already has is a duplicate.
	key string
	// The third column: 1 for English words, the preferred weight for words (fitted to the range words.txt uses when written), 0 for none (translations).
	weight int
	// How the commit message lists the entry.
	label string
}

type submission struct {
	kind  submissionKind
	lines []submissionLine
	note  string
	// flagged lists what the sensitive word list sent to review, for the commit message: entry labels with the matched category, or the note.
	flagged []string
}

// decodeEntries decodes the entries of one kind; an unknown field (for example a weight, or a field of another kind) is invalid JSON.
func decodeEntries[T any](raw json.RawMessage) ([]T, error) {
	var entries []T
	if len(raw) == 0 {
		return nil, nil
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func (s *Server) submitWords(w http.ResponseWriter, r *http.Request) {
	ws := s.words
	if ws == nil {
		wordsFail(w, 503, "word_submissions_disabled", "词条提交暂未开放，请稍后再试。")
		return
	}
	if !slices.Contains(ws.origins, r.Header.Get("Origin")) {
		wordsFail(w, 403, "origin_required", "请从官网表单提交。")
		return
	}
	address := ws.clientAddress(r)
	// A cheap gate in front of Turnstile so a script cannot make this server call siteverify without bound. It is shared by every replica through PostgreSQL, so round-robin across replicas does not multiply it.
	switch err := s.limitShared(r.Context(), "word-submissions", address, 10); {
	case errors.Is(err, account.ErrLimited):
		w.Header().Set("Retry-After", "60")
		wordsFail(w, 429, "rate_limit_exceeded", "提交过于频繁，请稍后再试。")
		return
	case err != nil:
		slog.Error("word submissions: rate limit unavailable", "reason", err.Error())
		w.Header().Set("Retry-After", "30")
		wordsFail(w, 503, "rate_limit_unavailable", "词条提交暂时不可用，请稍后再试。")
		return
	}
	if ct := strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]); ct != "application/json" {
		wordsFail(w, 415, "json_required", "请求格式不正确。")
		return
	}
	var input wordSubmissionRequest
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, wordSubmissionBodyBytes))
	d.DisallowUnknownFields()
	err := d.Decode(&input)
	if err == nil && d.Decode(new(any)) != io.EOF {
		err = errors.New("trailing data")
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		wordsFail(w, 413, "request_too_large", "内容过长。")
		return
	}
	if err != nil {
		wordsFail(w, 400, "invalid_json", "请求格式不正确。")
		return
	}
	kind := kindWords
	switch input.Kind {
	case "", kindWords.name:
	case kindEnglish.name:
		kind = kindEnglish
	case kindTranslations.name:
		kind = kindTranslations
	default:
		wordsFail(w, 400, "invalid_kind", "不支持的提交类型。")
		return
	}
	var (
		words        []wordSubmissionEntry
		english      []englishSubmissionEntry
		translations []translationSubmissionEntry
		count        int
	)
	switch kind {
	case kindWords:
		words, err = decodeEntries[wordSubmissionEntry](input.Entries)
		count = len(words)
	case kindEnglish:
		english, err = decodeEntries[englishSubmissionEntry](input.Entries)
		count = len(english)
	default:
		translations, err = decodeEntries[translationSubmissionEntry](input.Entries)
		count = len(translations)
	}
	if err != nil {
		wordsFail(w, 400, "invalid_json", "请求格式不正确。")
		return
	}
	if count < 1 || count > wordSubmissionMaxEntries {
		wordsFail(w, 400, "invalid_entry_count", "每次提交 1 到 20 个词条。")
		return
	}
	note, ok := sanitizeWordNote(input.Note)
	if !ok {
		wordsFail(w, 400, "invalid_note", "备注最多 500 个字。")
		return
	}
	var rejected []wordRejection
	switch kind {
	case kindWords:
		rejected = validateWordEntries(words)
	case kindEnglish:
		rejected = validateEnglishEntries(english)
	default:
		rejected = validateTranslationEntries(translations)
	}
	if len(rejected) > 0 {
		respond(w, 400, map[string]any{"error": "部分词条未通过校验，请修改后再提交。", "code": "invalid_entries", "rejected": rejected})
		return
	}
	if input.Token == "" || len(input.Token) > wordSubmissionTokenBytes {
		wordsFail(w, 400, "token_required", "请先完成提交验证。")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), wordSubmissionTimeout)
	defer cancel()
	if err = ws.verifyTurnstile(ctx, input.Token); err != nil {
		if errors.Is(err, errTurnstileRejected) {
			wordsFail(w, 403, "verification_failed", "验证已失效，请重新验证后提交。")
		} else {
			slog.Warn("word submissions: turnstile unavailable", "reason", err.Error())
			w.Header().Set("Retry-After", "30")
			wordsFail(w, 503, "verification_unavailable", "验证服务暂时不可用，请重新验证后再试。")
		}
		return
	}
	for _, l := range wordSubmissionLimits {
		if err = ws.limiter.RateLimit(ctx, l.scope, address, l.limit, l.window); err != nil {
			if errors.Is(err, account.ErrLimited) {
				w.Header().Set("Retry-After", l.retryAfter)
				wordsFail(w, 429, "rate_limit_exceeded", "提交过于频繁，请稍后再试。")
			} else {
				slog.Error("word submissions: rate limit unavailable", "reason", err.Error())
				w.Header().Set("Retry-After", "30")
				wordsFail(w, 503, "rate_limit_unavailable", "词条提交暂时不可用，请稍后再试。")
			}
			return
		}
	}
	sub := submission{kind: kind, note: note}
	if s.accounts != nil {
		var blocked []wordRejection
		blocked, sub.flagged, err = screenSubmission(ctx, s.accounts.Sensitive(), entryTexts(words, english, translations), note)
		switch {
		case errors.Is(err, errNoteBlocked):
			wordsFail(w, 400, "blocked_word", "备注包含不允许提交的内容，请修改后再提交。")
			return
		case err != nil:
			slog.Error("word submissions: sensitive word list unavailable", "reason", err.Error())
			w.Header().Set("Retry-After", "30")
			wordsFail(w, 503, "screening_unavailable", "词条提交暂时不可用，请稍后再试。")
			return
		case len(blocked) > 0:
			respond(w, 400, map[string]any{"error": "部分词条包含不允许提交的内容，请修改后再提交。", "code": "invalid_entries", "rejected": blocked})
			return
		}
	}
	switch kind {
	case kindWords:
		if err = s.shippedWords(ctx, words); err == nil {
			sub.lines = wordLines(words, ws.weightMedians(ctx, s.config.Engine))
		}
	case kindEnglish:
		if err = s.shippedEnglish(ctx, english); err == nil {
			sub.lines = englishLines(english)
		}
	default:
		sub.lines = translationLines(translations)
	}
	var number int
	if err == nil {
		number, err = ws.submit(ctx, sub)
	}
	var listed alreadyListedError
	switch {
	case err == nil:
		s.recordSubmission(ctx, number, kind, words, english, translations, note)
		respond(w, 201, map[string]string{"pull_request_url": "https://github.com/" + ws.config.GitHub.Repository + "/pull/" + strconv.Itoa(number)})
	case errors.As(err, &listed):
		rejected := make([]wordRejection, 0, len(listed))
		for _, index := range listed {
			rejected = append(rejected, wordRejection{index, "already_listed", kind.listed})
		}
		respond(w, 400, map[string]any{"error": "部分词条已在词库中。", "code": "invalid_entries", "rejected": rejected})
	case errors.Is(err, errWordsConflict):
		wordsFail(w, 409, "concurrent_update", "有其他人同时提交了词条，你的词条尚未写入。请重新验证后再次提交。")
	case errors.Is(err, errWordsUncertain):
		slog.Error("word submissions: GitHub write outcome unknown", "reason", err.Error())
		respond(w, 502, map[string]any{"error": "暂时无法确认提交结果。请先查看词库仓库中最新的 Pull Request，确认词条未写入后再提交。", "code": "outcome_unknown", "uncertain": true, "pulls_url": "https://github.com/" + ws.config.GitHub.Repository + "/pulls"})
	case errors.Is(err, errWordsBusy):
		if !errors.Is(err, account.ErrLockBusy) {
			slog.Error("word submissions: submission lock unavailable", "reason", err.Error())
		}
		w.Header().Set("Retry-After", "30")
		wordsFail(w, 503, "server_busy", "其他词条正在写入，你的词条尚未写入，请稍后再试。")
	case errors.Is(err, errWordsMisconfigured):
		slog.Error("word submissions: GitHub App misconfigured", "reason", err.Error())
		wordsFail(w, 503, "word_submissions_misconfigured", "词条提交暂未开放，请稍后再试。")
	default:
		slog.Error("word submissions: GitHub unavailable before any write", "reason", err.Error())
		w.Header().Set("Retry-After", "60")
		wordsFail(w, 503, "github_unavailable", "暂时无法读取词库仓库，词条尚未写入，请稍后再试。")
	}
}

// clientAddress 是按确定后的 `client_ip_header` 和官网代理密钥计算的访客地址（见 account.ClientAddress）。
func (ws *wordSubmitter) clientAddress(r *http.Request) string {
	return account.ClientAddress(r, ws.config.ClientIPHeader, ws.config.siteProxySecret)
}

func wordCharacter(r rune) bool { return unicode.Is(unicode.Unified_Ideograph, r) || r == '〇' }

// One rejection per entry (its first problem), in the same terms the website form uses, so the page can show each next to its row.
func validateWordEntries(entries []wordSubmissionEntry) []wordRejection {
	rejected := []wordRejection{}
	seen := map[string]bool{}
	for i, e := range entries {
		reject := func(code, reason string) { rejected = append(rejected, wordRejection{i, code, reason}) }
		characters := utf8.RuneCountInString(e.Word)
		switch {
		case e.Word == "":
			reject("word_required", "请填写词语")
			continue
		case !utf8.ValidString(e.Word) || strings.IndexFunc(e.Word, func(r rune) bool { return !wordCharacter(r) }) >= 0:
			reject("invalid_word", "词语只能包含汉字，不能有字母、数字、标点或空格")
			continue
		case characters > wordSubmissionMaxChars:
			reject("word_too_long", "词语最多 16 个汉字")
			continue
		}
		if e.Pinyin == "" {
			reject("pinyin_required", "请填写拼音")
			continue
		}
		syllables := strings.Split(e.Pinyin, "'")
		if len(e.Pinyin) > 200 || slices.ContainsFunc(syllables, func(s string) bool {
			return s == "" || strings.IndexFunc(s, func(r rune) bool { return r < 'a' || r > 'z' }) >= 0
		}) {
			reject("invalid_pinyin", "拼音只能包含小写字母，音节之间用 ' 分隔，例如 wei'lai'ke'qi")
			continue
		}
		if bad := slices.IndexFunc(syllables, func(s string) bool { return !pinyinSyllables[s] }); bad >= 0 {
			reject("invalid_syllable", "“"+syllables[bad]+"”不是有效的全拼音节（ü 请写作 v，如 lv、nve）")
			continue
		}
		if len(syllables) != characters {
			reject("syllable_count_mismatch", "“"+e.Word+"”有 "+strconv.Itoa(characters)+" 个字，但拼音有 "+strconv.Itoa(len(syllables))+" 个音节")
			continue
		}
		key := e.Word + "\t" + e.Pinyin
		if seen[key] {
			reject("duplicate_entry", "“"+e.Word+"”重复填写了")
			continue
		}
		seen[key] = true
	}
	return rejected
}

// plainText reports whether s is valid UTF-8 without control, format (zero-width, bidi) or line and paragraph separator characters, so it stays one field of one line and reads as what it looks like.
func plainText(s string) bool {
	return utf8.ValidString(s) && strings.IndexFunc(s, func(r rune) bool {
		return unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) || r == utf8.RuneError
	}) < 0
}

// validateEnglishEntries checks English words and trims their display forms in place. The word is the english_words key the Engine completes typed prefixes against: lowercase ASCII letters only.
func validateEnglishEntries(entries []englishSubmissionEntry) []wordRejection {
	rejected := []wordRejection{}
	seen := map[string]bool{}
	for i := range entries {
		e := &entries[i]
		reject := func(code, reason string) { rejected = append(rejected, wordRejection{i, code, reason}) }
		e.Display = strings.TrimSpace(e.Display)
		switch {
		case e.Word == "":
			reject("word_required", "请填写英文单词")
		case strings.IndexFunc(e.Word, func(r rune) bool { return r < 'a' || r > 'z' }) >= 0:
			reject("invalid_word", "单词是输入时键入的编码，只能包含小写英文字母 a–z，不能有大写、数字、空格或符号")
		case len(e.Word) > englishWordMaxBytes:
			reject("word_too_long", "单词最多 64 个字母")
		case e.Display == "":
			reject("display_required", "请填写候选中显示的词形")
		case !plainText(e.Display):
			reject("invalid_display", "显示词形不能包含制表符、换行或其他控制字符")
		case utf8.RuneCountInString(e.Display) > englishDisplayMaxChars:
			reject("display_too_long", "显示词形最多 64 个字符")
		case seen[e.Word+"\t"+e.Display]:
			reject("duplicate_entry", "“"+e.Display+"”重复填写了")
		default:
			seen[e.Word+"\t"+e.Display] = true
		}
	}
	return rejected
}

// validateTranslationEntries checks translation overrides and trims both sides in place, as the dictionary build strips them. A source may repeat one already in translations.txt (the later line wins, which is the point of an override); only an identical pair is a duplicate.
func validateTranslationEntries(entries []translationSubmissionEntry) []wordRejection {
	rejected := []wordRejection{}
	seen := map[string]bool{}
	for i := range entries {
		e := &entries[i]
		reject := func(code, reason string) { rejected = append(rejected, wordRejection{i, code, reason}) }
		e.Source, e.Gloss = strings.TrimSpace(e.Source), strings.TrimSpace(e.Gloss)
		switch {
		case e.Source == "":
			reject("source_required", "请填写原词")
		case !plainText(e.Source):
			reject("invalid_source", "原词不能包含制表符、换行或其他控制字符")
		case strings.HasPrefix(e.Source, "#"):
			reject("invalid_source", "原词不能以 # 开头")
		case utf8.RuneCountInString(e.Source) > translationSourceChars:
			reject("source_too_long", "原词最多 64 个字符")
		case e.Gloss == "":
			reject("gloss_required", "请填写译文")
		case !plainText(e.Gloss):
			reject("invalid_gloss", "译文不能包含制表符、换行或其他控制字符")
		case utf8.RuneCountInString(e.Gloss) > translationGlossChars:
			reject("gloss_too_long", "译文最多 200 个字符")
		case seen[e.Source+"\t"+e.Gloss]:
			reject("duplicate_entry", "“"+e.Source+"”的这条翻译重复填写了")
		default:
			seen[e.Source+"\t"+e.Gloss] = true
		}
	}
	return rejected
}

// The note ends up in a public commit message. It is flattened to one line, and every @ (mention), # and GH- (issue references, including closing keywords) and :// (autolinks) is broken with a zero-width space so a submission cannot ping people, touch issues or plant links.
func sanitizeWordNote(note string) (string, bool) {
	if !utf8.ValidString(note) || utf8.RuneCountInString(note) > wordSubmissionNoteChars {
		return "", false
	}
	note = strings.Map(func(r rune) rune {
		switch {
		case unicode.Is(unicode.Cc, r), r == ' ', r == ' ':
			return ' '
		case unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, note)
	note = strings.Join(strings.Fields(note), " ")
	return githubReferenceBreaker.Replace(note), true
}

var errTurnstileRejected = errors.New("turnstile rejected the token")

func (ws *wordSubmitter) verifyTurnstile(ctx context.Context, token string) error {
	form := url.Values{"secret": {ws.config.Turnstile.secret}, "response": {token}}
	req, err := http.NewRequestWithContext(ctx, "POST", ws.config.Turnstile.SiteverifyURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := ws.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return errors.New("siteverify returned status " + strconv.Itoa(resp.StatusCode))
	}
	var result struct {
		Success  bool   `json:"success"`
		Action   string `json:"action"`
		Hostname string `json:"hostname"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&result); err != nil {
		return errors.New("siteverify returned an invalid response")
	}
	if !result.Success || result.Action != "words" || !slices.Contains(ws.hostnames, result.Hostname) {
		return errTurnstileRejected
	}
	return nil
}

// submissionTitle names what the rolling pull request adds, per kind in title order with zero kinds left out, for example "feat(custom): add 3 words, 1 English word and 2 translations". It is also the squash commit subject that release-please reads.
func submissionTitle(counts map[string]int) string {
	var parts []string
	for _, kind := range submissionKinds {
		switch n := counts[kind.name]; {
		case n == 1:
			parts = append(parts, "1 "+kind.singular)
		case n > 1:
			parts = append(parts, strconv.Itoa(n)+" "+kind.plural)
		}
	}
	switch len(parts) {
	case 0:
		return "feat(custom): add community submissions"
	case 1:
		return "feat(custom): add " + parts[0]
	}
	return "feat(custom): add " + strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// addedLines counts the entry lines in head that base does not have, so maintainer edits on the rolling branch are reflected. Blank and # comment lines are not entries, and surrounding whitespace does not make a line different, as the dictionary build strips it.
func addedLines(base, head string) int {
	remaining := map[string]int{}
	entries := func(content string, visit func(string)) {
		for _, line := range strings.Split(content, "\n") {
			if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
				visit(line)
			}
		}
	}
	entries(base, func(line string) { remaining[line]++ })
	added := 0
	entries(head, func(line string) {
		if remaining[line] > 0 {
			remaining[line]--
		} else {
			added++
		}
	})
	return added
}

// Commit messages and titles are public. Contributed text is flattened by validation; this breaks every @ (mention), # and GH- (issue references, including closing keywords) and :// (autolinks) with a zero-width space so a submission cannot ping people, touch issues or plant links.
var githubReferenceBreaker = strings.NewReplacer("@", "@\u200b", "#", "#\u200b", "://", ":\u200b//", "GH-", "GH\u200b-", "gh-", "gh\u200b-", "Gh-", "Gh\u200b-", "gH-", "gH\u200b-")

// submissionCommitMessage titles the commit with this submission alone and lists its entries; the note follows on its own line after sanitising.
func submissionCommitMessage(sub submission) string {
	var b strings.Builder
	b.WriteString(submissionTitle(map[string]int{sub.kind.name: len(sub.lines)}) + "\n\n")
	for _, line := range sub.lines {
		b.WriteString("- " + line.label + "\n")
	}
	if len(sub.flagged) > 0 {
		b.WriteString("\nFlagged for review by the sensitive word list: " + strings.Join(sub.flagged, "; ") + "\n")
	}
	b.WriteString("\nSubmitted anonymously through the MSIME website form.\n")
	if sub.note != "" {
		b.WriteString("\nNote: " + sub.note + "\n")
	}
	return b.String()
}

// wordWeightRange is the lowest and highest weight of the entries in words.txt, which the msime-dictionary check-words gate requires every added entry to stay within; ok is false when the file has none. Lines are read as the dictionary build reads them: stripped, blank and # lines skipped, word<TAB>pinyin<TAB>weight.
func wordWeightRange(content string) (low, high int, ok bool) {
	for _, line := range strings.Split(content, "\n") {
		if line = strings.TrimSpace(line); line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			continue
		}
		weight, err := strconv.Atoi(strings.TrimSpace(fields[2]))
		if err != nil || weight < 1 {
			continue
		}
		if !ok {
			low, high, ok = weight, weight, true
		}
		low, high = min(low, weight), max(high, weight)
	}
	return low, high, ok
}

// appendSubmissionLines adds the submission's lines to content, making sure the existing content ends with a newline first. Word weights are fitted to the range base words.txt uses (and never below 1, the build's minimum).
func appendSubmissionLines(content string, sub submission, base string) string {
	low, high, ok := wordWeightRange(base)
	if !ok {
		low, high = 1, math.MaxInt
	}
	var b strings.Builder
	b.WriteString(content)
	if content != "" && !strings.HasSuffix(content, "\n") {
		b.WriteString("\n")
	}
	for _, line := range sub.lines {
		b.WriteString(line.key)
		switch {
		case sub.kind == kindWords:
			b.WriteString("\t" + strconv.Itoa(max(1, min(max(line.weight, low), high))))
		case line.weight > 0:
			b.WriteString("\t" + strconv.Itoa(line.weight))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// wordLines gives each word the base dictionary's median weight for its syllable count (8 standing for 8 or more), or the fallback weight when the medians are unknown.
func wordLines(entries []wordSubmissionEntry, medians map[int]int) []submissionLine {
	lines := make([]submissionLine, len(entries))
	for i, e := range entries {
		weight, ok := medians[min(strings.Count(e.Pinyin, "'")+1, 8)]
		if !ok {
			weight = wordSubmissionFallbackWeight
		}
		lines[i] = submissionLine{e.Word + "\t" + e.Pinyin, weight, e.Word + " " + e.Pinyin}
	}
	return lines
}

func englishLines(entries []englishSubmissionEntry) []submissionLine {
	lines := make([]submissionLine, len(entries))
	for i, e := range entries {
		lines[i] = submissionLine{e.Word + "\t" + e.Display, englishSubmissionWeight, githubReferenceBreaker.Replace(e.Word + " → " + e.Display)}
	}
	return lines
}

func translationLines(entries []translationSubmissionEntry) []submissionLine {
	lines := make([]submissionLine, len(entries))
	for i, e := range entries {
		lines[i] = submissionLine{e.Source + "\t" + e.Gloss, 0, githubReferenceBreaker.Replace(e.Source + " → " + e.Gloss)}
	}
	return lines
}

// weightMedians returns the base dictionary's median weight per syllable count (8 standing for 8 or more syllables), computed by the engine once per process. A failed computation is not cached, so the next submission tries again; meanwhile words get wordSubmissionFallbackWeight, which is logged only the first time.
func (ws *wordSubmitter) weightMedians(ctx context.Context, e engine.Config) map[int]int {
	ws.mediansMu.Lock()
	defer ws.mediansMu.Unlock()
	if ws.medians != nil {
		return ws.medians
	}
	raw, err := e.Query(ctx, map[string]any{"operation": "pinyin_weight_medians"})
	var out struct {
		Medians map[string]int `json:"medians"`
	}
	if err == nil {
		err = json.Unmarshal(raw, &out)
	}
	medians := map[int]int{}
	for key, weight := range out.Medians {
		if n, convErr := strconv.Atoi(key); convErr == nil && n >= 1 && n <= 8 {
			medians[n] = weight
		}
	}
	if err == nil && len(medians) == 0 {
		err = errors.New("no medians")
	}
	if err != nil {
		if !ws.mediansWarned {
			ws.mediansWarned = true
			slog.Warn("word submissions: base dictionary weight medians unavailable, using the fallback weight", "fallback", wordSubmissionFallbackWeight, "reason", err.Error())
		}
		return nil
	}
	ws.medians = medians
	return medians
}

type alreadyListedError []int

func (alreadyListedError) Error() string { return "entries already listed" }

// shippedWords rejects entries the base dictionary already holds, before GitHub is touched. The engine resources are the dictionary release the clients pin (third_party/msime), the same one the msime-dictionary check-words gate reads, so the two agree; an unavailable engine only logs and lets the submission through.
func (s *Server) shippedWords(ctx context.Context, entries []wordSubmissionEntry) error {
	batch := make([]map[string]string, len(entries))
	for i, e := range entries {
		batch[i] = map[string]string{"code": e.Pinyin, "word": e.Word}
	}
	return s.shippedEntries(ctx, "listed_pinyin_batch", batch)
}

// shippedEnglish rejects English words whose (word, display) pair english.db already holds. Nothing gates custom/english.txt in msime-dictionary, so maintainers review what an unavailable Engine lets through.
func (s *Server) shippedEnglish(ctx context.Context, entries []englishSubmissionEntry) error {
	batch := make([]map[string]string, len(entries))
	for i, e := range entries {
		batch[i] = map[string]string{"word": e.Word, "display": e.Display}
	}
	return s.shippedEntries(ctx, "listed_english_batch", batch)
}

// shippedEntries runs a read-only Engine lookup that answers one listed flag per entry. Any Engine failure skips the check with a warning.
func (s *Server) shippedEntries(ctx context.Context, operation string, batch []map[string]string) error {
	raw, err := s.config.Engine.Query(ctx, map[string]any{"operation": operation, "entries": batch})
	var out struct {
		Listed []bool `json:"listed"`
	}
	if err == nil {
		err = json.Unmarshal(raw, &out)
	}
	if err == nil && len(out.Listed) != len(batch) {
		err = errors.New("listed count mismatch")
	}
	if err != nil {
		slog.Warn("word submissions: base dictionary check skipped", "operation", operation, "reason", err.Error())
		return nil
	}
	var listed alreadyListedError
	for i, found := range out.Listed {
		if found {
			listed = append(listed, i)
		}
	}
	if len(listed) == 0 {
		return nil
	}
	return listed
}

// listedLines returns the indexes of lines whose key (the first two columns) content already has. Columns are stripped and blank and # lines skipped, as the dictionary build reads the files.
func listedLines(content string, lines []submissionLine) alreadyListedError {
	existing := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if fields := strings.Split(line, "\t"); len(fields) >= 2 {
			existing[strings.TrimSpace(fields[0])+"\t"+strings.TrimSpace(fields[1])] = true
		}
	}
	var listed alreadyListedError
	for i, line := range lines {
		if existing[line.key] {
			listed = append(listed, i)
		}
	}
	return listed
}

// errNoteBlocked is a submission whose note hits a block-level sensitive word.
var errNoteBlocked = errors.New("note hits a blocked sensitive word")

// entryTexts is the text of each entry the sensitive word list screens: the word, or both columns of an English word or translation. Each column is matched on its own, so a match never spans two columns.
func entryTexts(words []wordSubmissionEntry, english []englishSubmissionEntry, translations []translationSubmissionEntry) [][]string {
	var texts [][]string
	for _, e := range words {
		texts = append(texts, []string{e.Word})
	}
	for _, e := range english {
		texts = append(texts, []string{e.Word, e.Display})
	}
	for _, e := range translations {
		texts = append(texts, []string{e.Source, e.Gloss})
	}
	return texts
}

// screenSubmission matches every column of every entry and the note against the sensitive word list. Entries with a block hit come back as blocked_word rejections, a note with one as errNoteBlocked, and review hits as the flags the commit message carries (the entry's position and the matched categories, never the configured patterns, which stay private).
func screenSubmission(ctx context.Context, matcher account.SensitiveMatcher, texts [][]string, note string) ([]wordRejection, []string, error) {
	var blocked []wordRejection
	var flagged []string
	// screen matches the columns of one entry; it reports whether any hit blocks, and the categories of the hits otherwise.
	screen := func(columns []string) (bool, []string, error) {
		var categories []string
		for _, column := range columns {
			if strings.TrimSpace(column) == "" {
				continue
			}
			hits, err := matcher.Match(ctx, column)
			if err != nil {
				return false, nil, err
			}
			for _, h := range hits {
				if h.Level == account.SensitiveBlock {
					return true, nil, nil
				}
				if !slices.Contains(categories, h.Category) {
					categories = append(categories, h.Category)
				}
			}
		}
		return false, categories, nil
	}
	for i, columns := range texts {
		block, categories, err := screen(columns)
		switch {
		case err != nil:
			return nil, nil, err
		case block:
			blocked = append(blocked, wordRejection{i, "blocked_word", "包含不允许提交的内容"})
		case len(categories) > 0:
			flagged = append(flagged, "entry "+strconv.Itoa(i+1)+" ("+strings.Join(categories, ", ")+")")
		}
	}
	block, categories, err := screen([]string{note})
	switch {
	case err != nil:
		return nil, nil, err
	case block:
		return nil, nil, errNoteBlocked
	case len(categories) > 0:
		flagged = append(flagged, "note ("+strings.Join(categories, ", ")+")")
	}
	return blocked, flagged, nil
}

// recordSubmission stores a submission GitHub accepted, for the dictionary review page. The entries are already public on the pull request, so a failed write is only logged: answering with an error would make the visitor submit them again. It runs detached from the request so a visitor leaving does not cancel it.
func (s *Server) recordSubmission(ctx context.Context, number int, kind submissionKind, words []wordSubmissionEntry, english []englishSubmissionEntry, translations []translationSubmissionEntry, note string) {
	if s.accounts == nil {
		return
	}
	var entries any
	switch kind {
	case kindWords:
		entries = words
	case kindEnglish:
		entries = english
	default:
		entries = translations
	}
	raw, err := json.Marshal(entries)
	if err == nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		err = s.accounts.RecordWordSubmission(ctx, account.WordSubmission{PRNumber: number, Kind: kind.name, Entries: raw, Note: note})
	}
	if err != nil {
		slog.Error("word submissions: submission not recorded", "pull", number, "reason", err.Error())
	}
}
