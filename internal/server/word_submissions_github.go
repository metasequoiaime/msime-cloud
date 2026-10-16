package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

// Outcomes of the GitHub part of a submission. Anything that fails before the first write that could carry entries is errWordsUnavailable (nothing was submitted, the visitor may simply try again). A write that GitHub may or may not have applied is errWordsUncertain and is never retried, because a retry could duplicate the entries.
var (
	errWordsConflict      = errors.New("dictionary file changed concurrently")
	errWordsUncertain     = errors.New("GitHub write outcome unknown")
	errWordsUnavailable   = errors.New("GitHub unavailable")
	errWordsMisconfigured = errors.New("GitHub App credentials rejected")
	// Another replica's submission still held the cross-replica lock after wordSubmissionLockWait, or the database could not take it; nothing was read or written.
	errWordsBusy = errors.New("another submission is being written")
)

const wordsPullRequestBody = `This is the rolling pull request for dictionary entries submitted anonymously through the forms on the MSIME website (msime.app). Each commit on this branch is one submission of one kind; its commit message lists the entries together with the submitter's note, if any.

- Words append ` + "`word<TAB>pinyin<TAB>weight`" + ` lines to ` + "`custom/words.txt`" + `. The weight is the median weight of the base dictionary's entries with the same number of syllables, kept within the range words.txt already uses.
- English words append ` + "`word<TAB>display<TAB>1`" + ` lines to ` + "`custom/english.txt`" + `, where word is the lowercase key that is typed and display the form the candidate shows. No dictionary build reads this file yet.
- Translations append ` + "`source<TAB>gloss`" + ` lines to ` + "`custom/translations.txt`" + `. A later line for the same source overrides an earlier one, so a submission may correct an existing translation.

- Submissions are opened by the MSIME word-submission GitHub App after a Cloudflare Turnstile check and a per-address rate limit. No account or personal data is collected.
- Every entry is reviewed by maintainers before merge. Remove or fix entries on this branch as needed; new submissions keep appending here while this pull request is open.
- The msime-dictionary CI validates the format, readings, weights and duplicates of words.txt additions. English words and translations are checked by review only.
- Squash-merge this pull request. Its title is kept at what this branch adds over the base branch (for example ` + "`feat(custom): add 3 words, 1 English word and 2 translations`" + `) and becomes the commit that release-please turns into the next ` + "`sources-v*`" + ` release.
- Merging here does not ship the entries by itself. They reach users once msime moves its custom-dictionary pin in ` + "`resources/dictionary-sources.lock.json`" + ` to a msime-dictionary commit that contains them and cuts the next ` + "`dict-v*`" + ` release on metasequoiaime/msime with ` + "`release-dictionary.yml`" + `; msime's desktop builds take that release.
`

type githubResponse struct {
	status int
	body   []byte
}

// app is the GitHub App the submitter writes as. It is rebuilt from the configuration on every call so the HTTP client and clock stay replaceable, while minted tokens live in the submitter's shared cache.
func (ws *wordSubmitter) app() githubapp.Client {
	g := ws.config.GitHub
	return githubapp.Client{AppID: g.AppID, InstallationID: g.InstallationID, Key: g.key, APIURL: g.APIURL, UserAgent: "MSIME-Backend-word-submissions", HTTP: ws.client, Now: ws.now, Cache: &ws.github}
}

// githubRequest performs one GitHub REST call. A transport failure is returned as an error with a zero status; HTTP errors are returned as a status for the caller to classify.
func (ws *wordSubmitter) githubRequest(ctx context.Context, token, method, path string, body any) (githubResponse, error) {
	r, err := ws.app().Do(ctx, token, method, path, body)
	return githubResponse{r.Status, r.Body}, err
}

func (r githubResponse) ok() bool { return r.status >= 200 && r.status < 300 }

func phaseError(kind error, phase string, r githubResponse, cause error) error {
	detail := phase + ": status " + strconv.Itoa(r.status)
	if cause != nil {
		detail = phase + ": " + cause.Error()
	}
	return errors.Join(kind, errors.New(detail))
}

// installationToken mints (and caches until shortly before expiry) an installation token restricted to the target repository with only contents:write and pull_requests:write, whatever else the installation may have been granted.
func (ws *wordSubmitter) installationToken(ctx context.Context) (string, error) {
	token, err := ws.app().Token(ctx, ws.config.GitHub.Repository, map[string]string{"contents": "write", "pull_requests": "write"})
	switch {
	case err == nil:
		return token, nil
	case errors.Is(err, githubapp.ErrRejected):
		return "", errors.Join(errWordsMisconfigured, err)
	default:
		return "", errors.Join(errWordsUnavailable, err)
	}
}

type githubPull struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Head   struct {
		Ref  string `json:"ref"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

type githubFile struct {
	Type     string `json:"type"`
	SHA      string `json:"sha"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
}

func decodeGitHubBase64(content string, raw []byte) (int, error) {
	const blockSize = 4096
	var block [blockSize]byte
	blockLen, written := 0, 0
	finished := false
	flush := func() error {
		n, err := base64.StdEncoding.Decode(raw[written:], block[:blockLen])
		written += n
		if err != nil {
			return err
		}
		for _, c := range block[:blockLen] {
			if c == '=' {
				finished = true
				break
			}
		}
		blockLen = 0
		return nil
	}
	for i := 0; i < len(content); {
		c := content[i]
		if c < utf8.RuneSelf {
			i++
			switch c {
			case '\t', '\n', '\v', '\f', '\r', ' ':
				continue
			}
		} else {
			runeValue, size := utf8.DecodeRuneInString(content[i:])
			if unicode.IsSpace(runeValue) {
				i += size
				continue
			}
			i++
		}
		if finished {
			return written, errors.New("trailing base64 data")
		}
		block[blockLen] = c
		blockLen++
		if blockLen == blockSize {
			if err := flush(); err != nil {
				return written, err
			}
		}
	}
	if blockLen > 0 {
		if err := flush(); err != nil {
			return written, err
		}
	}
	return written, nil
}

func (f githubFile) text() (string, bool) {
	if f.Type != "file" || f.Encoding != "base64" || f.SHA == "" {
		return "", false
	}
	// 解码器会忽略 CR/LF；只按实际编码字节数预留缓冲，避免为空行分配内存。
	content := f.Content
	encodedSize := 0
	needsFilter := false
	for i := 0; i < len(content); {
		c := content[i]
		if c < utf8.RuneSelf {
			i++
			switch c {
			case '\r', '\n':
				continue
			case '\t', '\v', '\f', ' ':
				needsFilter = true
				continue
			}
			encodedSize++
			continue
		}
		runeValue, size := utf8.DecodeRuneInString(content[i:])
		i += size
		needsFilter = true
		if !unicode.IsSpace(runeValue) {
			encodedSize += size
		}
	}
	raw := make([]byte, base64.StdEncoding.DecodedLen(encodedSize))
	var n int
	var err error
	if needsFilter {
		n, err = decodeGitHubBase64(content, raw)
	} else {
		n, err = base64.StdEncoding.Decode(raw, []byte(content))
	}
	raw = raw[:n]
	return string(raw), err == nil && utf8.Valid(raw)
}

// submit appends the submission to its file on the open rolling pull request, or on a new community-words/<UTC timestamp> branch with a new pull request when none is open, and returns the pull request number. Every kind shares that one branch and pull request. Branch names carry a timestamp, so a merged or closed pull request is never reused. It never deletes, force-pushes or retries a write.
func (ws *wordSubmitter) submit(ctx context.Context, sub submission) (int, error) {
	ws.writes.Lock()
	defer ws.writes.Unlock()
	// Without this lock two replicas that both find no open pull request each create a community-words/<UTC second> branch and open a pull request of their own; the blob SHA check cannot catch that because the branches differ. The in-process mutex above stays in front so a replica queues its own requests locally and holds at most one database connection for the lock.
	if ws.locks != nil {
		release, err := ws.locks.WaitLock(ctx, "word-submissions:"+strings.ToLower(ws.config.GitHub.Repository), wordSubmissionLockWait)
		if err != nil {
			return 0, errors.Join(errWordsBusy, err)
		}
		defer release()
	}
	token, err := ws.installationToken(ctx)
	if err != nil {
		return 0, err
	}
	g := ws.config.GitHub
	repo := "/repos/" + g.Repository
	read := func(phase, path string, out any) error {
		r, err := ws.githubRequest(ctx, token, "GET", repo+path, nil)
		if err != nil || !r.ok() {
			return phaseError(errWordsUnavailable, phase, r, err)
		}
		if err = json.Unmarshal(r.body, out); err != nil {
			return phaseError(errWordsUnavailable, phase, r, errors.New("invalid response"))
		}
		return nil
	}
	readText := func(phase, ref string) (githubFile, string, error) {
		var file githubFile
		if err := read(phase, "/contents/"+sub.kind.file+"?"+url.Values{"ref": {ref}}.Encode(), &file); err != nil {
			return file, "", err
		}
		content, ok := file.text()
		if !ok {
			return file, "", phaseError(errWordsUnavailable, phase, githubResponse{status: 200}, errors.New("unexpected content encoding"))
		}
		return file, content, nil
	}
	var pulls []githubPull
	if err = read("list pull requests", "/pulls?"+url.Values{"state": {"open"}, "base": {g.Branch}, "per_page": {"100"}, "sort": {"created"}, "direction": {"asc"}}.Encode(), &pulls); err != nil {
		return 0, err
	}
	var open *githubPull
	for i := range pulls {
		p := &pulls[i]
		// Only branches in the target repository itself: a fork can name its branch community-words/... too.
		if strings.HasPrefix(p.Head.Ref, wordSubmissionBranches) && p.Head.Repo != nil && strings.EqualFold(p.Head.Repo.FullName, g.Repository) && p.Base.Ref == g.Branch {
			open = p
			break
		}
	}
	branch, readRef := "", ""
	if open != nil {
		branch, readRef = open.Head.Ref, open.Head.Ref
	} else {
		var base struct {
			Object struct {
				SHA string `json:"sha"`
			} `json:"object"`
		}
		if err = read("read base branch", "/git/ref/heads/"+g.Branch, &base); err != nil {
			return 0, err
		}
		if base.Object.SHA == "" {
			return 0, phaseError(errWordsUnavailable, "read base branch", githubResponse{status: 200}, errors.New("missing commit SHA"))
		}
		branch, readRef = wordSubmissionBranches+ws.now().UTC().Format("20060102-150405"), base.Object.SHA
	}
	// The base branch's copy gives the weight range words.txt entries must keep, and what the rolling pull request adds for its title. A new branch starts at the base, so there the file just read is that copy.
	baseContent := ""
	if open != nil {
		if _, baseContent, err = readText("read base "+sub.kind.file, g.Branch); err != nil {
			return 0, err
		}
	}
	file, content, err := readText("read "+sub.kind.file, readRef)
	if err != nil {
		return 0, err
	}
	if open == nil {
		baseContent = content
	}
	// Checked before creating a branch so a rejected submission leaves nothing behind.
	if listed := listedLines(content, sub.lines); len(listed) > 0 {
		return 0, listed
	}
	if open == nil {
		// The branch starts at the commit the file was just read from, so the blob SHA below still matches.
		r, err := ws.githubRequest(ctx, token, "POST", repo+"/git/refs", map[string]string{"ref": "refs/heads/" + branch, "sha": readRef})
		switch {
		case err == nil && r.status == 422:
			// Another submission created the same branch name in the same second.
			return 0, phaseError(errWordsConflict, "create branch", r, nil)
		case err != nil || !r.ok():
			// At worst an empty branch exists; no entries were written, so trying again is safe.
			return 0, phaseError(errWordsUnavailable, "create branch", r, err)
		}
	}
	// The blob SHA makes this a compare-and-swap: if the file changed on the branch since it was read, GitHub rejects the write and the visitor is asked to submit again.
	updated := appendSubmissionLines(content, sub, baseContent)
	r, err := ws.githubRequest(ctx, token, "PUT", repo+"/contents/"+sub.kind.file, map[string]string{
		"message": submissionCommitMessage(sub),
		"content": base64.StdEncoding.EncodeToString([]byte(updated)),
		"sha":     file.SHA,
		"branch":  branch,
	})
	switch {
	case err != nil || r.status >= 500:
		return 0, phaseError(errWordsUncertain, "commit "+sub.kind.file, r, err)
	case r.status == 409 || r.status == 422:
		return 0, phaseError(errWordsConflict, "commit "+sub.kind.file, r, nil)
	case !r.ok():
		return 0, phaseError(errWordsUnavailable, "commit "+sub.kind.file, r, nil)
	}
	if open != nil {
		ws.retitle(ctx, token, *open, map[string]int{sub.kind.name: addedLines(baseContent, updated)})
		return open.Number, nil
	}
	// From here on the entries are committed, so failing to open the pull request is reported as uncertain rather than as not submitted.
	r, err = ws.githubRequest(ctx, token, "POST", repo+"/pulls", map[string]any{"title": submissionTitle(map[string]int{sub.kind.name: len(sub.lines)}), "head": branch, "base": g.Branch, "body": wordsPullRequestBody})
	var created struct {
		Number int `json:"number"`
	}
	if err != nil || !r.ok() || json.Unmarshal(r.body, &created) != nil || created.Number <= 0 {
		return 0, phaseError(errWordsUncertain, "open pull request", r, err)
	}
	return created.Number, nil
}

// retitle keeps the rolling pull request's title at what its branch adds over the base branch in every submission file; counts already holds the file just written. The entries are committed by now, so a failure here is only logged: the next submission corrects the title, and maintainers can still edit it before merging.
func (ws *wordSubmitter) retitle(ctx context.Context, token string, open githubPull, counts map[string]int) {
	for _, kind := range submissionKinds {
		if _, done := counts[kind.name]; done {
			continue
		}
		base, err := ws.fileText(ctx, token, kind.file, ws.config.GitHub.Branch)
		head := ""
		if err == nil {
			head, err = ws.fileText(ctx, token, kind.file, open.Head.Ref)
		}
		if err != nil {
			slog.Warn("word submissions: pull request title not updated", "pull", open.Number, "reason", err.Error())
			return
		}
		counts[kind.name] = addedLines(base, head)
	}
	title := submissionTitle(counts)
	if title == open.Title {
		return
	}
	r, err := ws.githubRequest(ctx, token, "PATCH", "/repos/"+ws.config.GitHub.Repository+"/pulls/"+strconv.Itoa(open.Number), map[string]string{"title": title})
	if err != nil || !r.ok() {
		slog.Warn("word submissions: pull request title not updated", "pull", open.Number, "status", r.status)
	}
}

// fileText reads a file at ref for counting only; a file the ref does not have reads as empty.
func (ws *wordSubmitter) fileText(ctx context.Context, token, path, ref string) (string, error) {
	r, err := ws.githubRequest(ctx, token, "GET", "/repos/"+ws.config.GitHub.Repository+"/contents/"+path+"?"+url.Values{"ref": {ref}}.Encode(), nil)
	switch {
	case err != nil:
		return "", err
	case r.status == 404:
		return "", nil
	case !r.ok():
		return "", errors.New("read " + path + ": status " + strconv.Itoa(r.status))
	}
	var file githubFile
	if json.Unmarshal(r.body, &file) != nil {
		return "", errors.New("read " + path + ": invalid response")
	}
	content, ok := file.text()
	if !ok {
		return "", errors.New("read " + path + ": unexpected content encoding")
	}
	return content, nil
}
