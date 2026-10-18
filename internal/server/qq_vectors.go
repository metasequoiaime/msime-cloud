package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"golang.org/x/sync/errgroup"
)

// 答疑的向量检索。目录挑段落只看得到标题和开头几十个字，答案埋在正文深处、或者群友的口语问法和文档说法对不上时挑不中；GitHub 搜索要求每个词都匹配，也搜不到意思相同、说法不同的旧 issue。所以把文档段落、答疑仓库全部的 issue 和 PR 都算成向量存进 PostgreSQL（pgvector，见 account/qq_knowledge.go），答疑时把问题也算成向量，按余弦相似度召回：文档段落并进模型挑出的段落，issue 和 PR 作为「语义相近的历史记录」交给回答的模型。
//
// 同步由一个副本做（auth_rates 认领，每 qqVectorSyncInterval 一次）：文档按段落内容的摘要比对，变了才重算；issue 和 PR 按仓库记下同步到的更新时间，每次从那里往后拉（GitHub 的 issues 接口同时返回 PR），首次全量回填分几次跑完，每次每个仓库最多 qqVectorSyncPages 页。数据库没装 pgvector、chat 端点不是 …/chat/completions 的形状，或者 embedding_model 写 none 时整个功能关闭，答疑照旧只用目录挑段落。

const (
	defaultQQEmbeddingModel = "doubao-embedding-vision"
	// qqEmbedBatch 是一次请求算的文本数：doubao-embedding-vision 一次最多 10 条（多了上游返回 400 「input limit exceeded: max 10」），10 条 1500 字的文本响应约 340KB，在上游响应 1MB 的上限内。
	qqEmbedBatch = 10
	// qqEmbedParallel 限制一次同步同时发出的 embedding 请求数，避免批量同步把上游并发额度打满。
	qqEmbedParallel = 4
	// qqEmbedRunes 是算向量时每段文本的长度上限。
	qqEmbedRunes = 1500
	// qqVectorSyncInterval 是同步的间隔，qqVectorSyncPages 是每次每个仓库最多拉的页数（每页 100 条）。
	qqVectorSyncInterval = 10 * time.Minute
	qqVectorSyncPages    = 20
	// qqVectorDocHits、qqVectorHistoryHits 是每次召回的文档段落数和 issue、PR 数；相似度低于 qqVectorMinSimilarity 的不要。
	qqVectorDocHits       = 6
	qqVectorHistoryHits   = 5
	qqVectorMinSimilarity = 0.45
	// qqHistorySummaryRunes 是交给模型的每条 issue、PR 摘要的正文长度上限。
	qqHistorySummaryRunes = 300
)

// qqHistoryPerms 是同步 issue 和 PR 时的令牌权限：只有 issues 读权限的令牌调 issues 接口时，GitHub 会把 PR 滤掉（曾经因此一条 PR 都没有入库），所以要加上 pull_requests 读权限。
var qqHistoryPerms = map[string]string{"contents": "read", "issues": "read", "pull_requests": "read", "metadata": "read"}

// qqVectorStore 是 account.Service 里的向量索引，所有副本共享；没有数据库时为 nil。
type qqVectorStore interface {
	QQVectorsReady(ctx context.Context) error
	QQKnowledgeHashes(ctx context.Context, ids []string) (map[string]string, error)
	UpsertQQKnowledge(ctx context.Context, items []account.QQKnowledgeItem) error
	DeleteQQKnowledgeDocs(ctx context.Context, keep []string) error
	SearchQQKnowledge(ctx context.Context, embedding []float32, kinds []string, limit int) ([]account.QQKnowledgeHit, error)
	QQKnowledgeCursor(ctx context.Context, repo string) (time.Time, error)
	SetQQKnowledgeCursor(ctx context.Context, repo string, since time.Time) error
}

// qqEmbeddingURL 是 embedding 接口的地址：把 chat 端点路径末尾的 chat/completions 换成 embeddings（EveryAPI 等 OpenAI 兼容服务都是这样）。功能关闭时返回空字符串。
func (s *Server) qqEmbeddingURL() string {
	if s.qq == nil || s.qq.config.EmbeddingModel == "none" {
		return ""
	}
	u, err := url.Parse(s.config.Chat.URL)
	if err != nil || u.Host == "" {
		return ""
	}
	u.Path = path.Join(strings.TrimSuffix(u.Path, "/chat/completions"), "embeddings")
	return u.String()
}

// qqEmbed 把 texts 算成向量，顺序与输入一致。
func (s *Server) qqEmbed(ctx context.Context, texts []string) ([][]float32, error) {
	endpoint := s.qqEmbeddingURL()
	if endpoint == "" {
		return nil, errors.New("embeddings disabled")
	}
	payload, _ := json.Marshal(map[string]any{"model": s.qq.config.EmbeddingModel, "input": texts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, errors.New("invalid embedding endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if token := s.config.Chat.token; token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	b, err := s.doUpstream(req)
	if err != nil {
		return nil, err
	}
	var result struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if json.Unmarshal(b, &result) != nil || len(result.Data) != len(texts) {
		return nil, errors.New("invalid embedding response")
	}
	vectors := make([][]float32, len(texts))
	for _, d := range result.Data {
		if d.Index < 0 || d.Index >= len(texts) || vectors[d.Index] != nil || len(d.Embedding) != account.QQEmbeddingDims {
			return nil, errors.New("invalid embedding response")
		}
		vectors[d.Index] = d.Embedding
	}
	return vectors, nil
}

// qqEmbedItems 给 items 算向量（texts 与 items 一一对应），分批请求。
func (s *Server) qqEmbedItems(ctx context.Context, items []account.QQKnowledgeItem, texts []string) error {
	if len(items) != len(texts) {
		return errors.New("embedding items and texts length mismatch")
	}
	results := make([][][]float32, (len(texts)+qqEmbedBatch-1)/qqEmbedBatch)
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(qqEmbedParallel)
	scheduled := false
	for batch, start := 0, 0; start < len(texts); batch, start = batch+1, start+qqEmbedBatch {
		if err := groupCtx.Err(); err != nil {
			break
		}
		batch, start := batch, start
		end := min(start+qqEmbedBatch, len(texts))
		scheduled = true
		group.Go(func() error {
			if err := groupCtx.Err(); err != nil {
				return err
			}
			vectors, err := s.qqEmbed(groupCtx, texts[start:end])
			if err != nil {
				return err
			}
			results[batch] = vectors
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return err
	}
	if !scheduled && ctx.Err() != nil {
		return ctx.Err()
	}
	for batch, vectors := range results {
		start := batch * qqEmbedBatch
		for i, v := range vectors {
			items[start+i].Embedding = v
		}
	}
	return nil
}

// qqTruncate 按 Unicode 标量截断并追加省略号，只扫描保留的前缀，避免为长正文分配全文 rune 切片。
func qqTruncate(text string, limit int) string {
	if limit < 0 {
		panic("negative truncate limit")
	}
	end := qqRuneEnd(text, limit)
	if end == len(text) {
		return text
	}
	prefix := text[:end]
	if !utf8.ValidString(prefix) {
		// 沿用截断时的 rune 转换语义，每个无效字节分别替换为 U+FFFD。
		prefix = string([]rune(prefix))
	}
	return prefix + "…"
}

// qqRuneEnd 返回保留 limit 个 Unicode 标量时的结束字节位置，不足时返回全文长度。
func qqRuneEnd(text string, limit int) int {
	count := 0
	for i := range text {
		if count == limit {
			return i
		}
		count++
	}
	return len(text)
}

// qqContentHash 分块计算以 NUL 分隔的原始字节哈希，避免拼接和转换时复制完整正文。
func qqContentHashSum(parts ...string) (sum [sha256.Size]byte) {
	small := true
	total := 0
	for i, part := range parts {
		remaining := 1024 - total
		if i > 0 {
			if remaining == 0 {
				small = false
				break
			}
			remaining--
		}
		if len(part) > remaining {
			small = false
			break
		}
		total += len(part)
		if i > 0 {
			total++
		}
	}
	if small {
		var buf [1024]byte
		used := 0
		for i, part := range parts {
			if i > 0 {
				buf[used] = 0
				used++
			}
			used += copy(buf[used:], part)
		}
		return sha256.Sum256(buf[:used])
	}
	h := sha256.New()
	var buf [1024]byte
	used := 0
	for i, part := range parts {
		if i > 0 {
			buf[used] = 0
			used++
			if used == len(buf) {
				h.Write(buf[:])
				used = 0
			}
		}
		for part != "" {
			n := copy(buf[used:], part)
			used += n
			part = part[n:]
			if used == len(buf) {
				h.Write(buf[:])
				used = 0
			}
		}
	}
	h.Write(buf[:used])
	h.Sum(sum[:0])
	return sum
}

func qqContentHash(parts ...string) string {
	sum := qqContentHashSum(parts...)
	var encoded [sha256.Size * 2]byte
	hex.Encode(encoded[:], sum[:])
	return string(encoded[:])
}

// qqDocID 是文档段落在向量索引里的 id：按文档名和标题，内容变了 id 不变。
func qqDocID(section qqSection) string {
	id := qqDocIDBytes(section)
	return string(id[:])
}

// qqDocIDBytes 返回固定长度的文档 ID，供临时查找使用，避免为每个段落保留字符串副本。
func qqDocIDBytes(section qqSection) (id [len("doc:") + 24]byte) {
	sum := qqContentHashSum(section.Doc, section.Title)
	copy(id[:], "doc:")
	hex.Encode(id[len("doc:"):], sum[:12])
	return id
}

// qqVectorJob 是同步向量索引的后台任务，运行到 s.lifetime 结束。
func (s *Server) qqVectorJob(ctx context.Context) {
	ticker := time.NewTicker(qqVectorSyncInterval)
	defer ticker.Stop()
	for {
		s.syncQQVectors(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// syncQQVectors 做一次同步：同一个间隔里只有认领成功的副本做。
func (s *Server) syncQQVectors(ctx context.Context) {
	if s.qqVectors == nil || s.qqEmbeddingURL() == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, qqVectorSyncInterval-time.Minute)
	defer cancel()
	if err := s.qqVectors.QQVectorsReady(ctx); err != nil {
		slog.Info("qq bot: vector index unavailable", "reason", err.Error())
		return
	}
	if err := s.limits.RateLimit(ctx, "qq-vector-sync", "all", 1, qqVectorSyncInterval-30*time.Second); err != nil {
		return
	}
	docs, err := s.syncQQDocVectors(ctx)
	if err != nil && ctx.Err() == nil {
		slog.Warn("qq bot: doc vectors not synced", "reason", err.Error())
	}
	history := 0
	for _, repo := range s.qqGitHubRepos() {
		n, err := s.syncQQHistoryVectors(ctx, repo)
		history += n
		if err != nil && ctx.Err() == nil {
			slog.Warn("qq bot: history vectors not synced", "repo", repo, "reason", err.Error())
		}
	}
	if docs+history > 0 {
		slog.Info("qq bot: vectors synced", "docs", docs, "history", history)
	}
}

// syncQQDocVectors 同步文档段落，返回重算的段落数。
func (s *Server) syncQQDocVectors(ctx context.Context) (int, error) {
	sections, err := s.qqKnowledgeSections(ctx)
	if err != nil {
		return 0, err
	}
	ids := make([]string, len(sections))
	uniqueEstimate := 0
	for i, section := range sections {
		if i > 0 && section.Doc == sections[i-1].Doc && section.Title == sections[i-1].Title {
			ids[i] = ids[i-1]
		} else {
			ids[i] = qqDocID(section)
		}
		if i == 0 || ids[i] != ids[i-1] {
			uniqueEstimate++
		}
	}
	hashes, err := s.qqVectors.QQKnowledgeHashes(ctx, ids)
	if err != nil {
		return 0, err
	}
	items := make([]account.QQKnowledgeItem, 0, uniqueEstimate)
	texts := make([]string, 0, uniqueEstimate)
	var pending map[string]struct{}
	for i, section := range sections {
		// 只对本轮已经选中更新的 ID 去重，未变化的段落不能遮住后续同 ID 的更新。
		if _, ok := pending[ids[i]]; ok {
			continue
		}
		text := "[" + section.Doc + "] " + section.Title + "\n" + qqTruncate(section.Text, qqEmbedRunes)
		h := qqContentHash(text)
		if hashes[ids[i]] == h {
			continue
		}
		if pending == nil {
			pending = make(map[string]struct{}, uniqueEstimate)
		}
		pending[ids[i]] = struct{}{}
		items = append(items, account.QQKnowledgeItem{ID: ids[i], Kind: "doc", Title: section.Doc + " · " + section.Title, Summary: qqTruncate(section.Text, qqHistorySummaryRunes), ContentHash: h, UpdatedAt: s.qq.now()})
		texts = append(texts, text)
	}
	if err := s.qqEmbedItems(ctx, items, texts); err != nil {
		return 0, err
	}
	if err := s.qqVectors.UpsertQQKnowledge(ctx, items); err != nil {
		return 0, err
	}
	return len(items), s.qqVectors.DeleteQQKnowledgeDocs(ctx, ids)
}

// qqHistoryIssue 是同步时从 issues 接口读到的一条 issue 或 PR。
type qqHistoryIssue struct {
	qqIssue
	qqIssueDetail
	UpdatedAt   time.Time `json:"updated_at"`
	PullRequest *struct {
		MergedAt string `json:"merged_at"`
	} `json:"pull_request"`
}

// syncQQHistoryVectors 从上次同步到的时间往后拉 repo 的 issue 和 PR，返回重算的条数。
func (s *Server) syncQQHistoryVectors(ctx context.Context, repo string) (int, error) {
	client := s.adminGitHub
	token, err := client.Token(ctx, repo, qqHistoryPerms)
	if err != nil {
		return 0, err
	}
	since, err := s.qqVectors.QQKnowledgeCursor(ctx, repo)
	if err != nil {
		return 0, err
	}
	_, short, _ := strings.Cut(repo, "/")
	synced := 0
	query := url.Values{"state": {"all"}, "sort": {"updated"}, "direction": {"asc"}, "per_page": {"100"}}
	if !since.IsZero() {
		query.Set("since", since.UTC().Format(time.RFC3339))
	}
	for page := 1; page <= qqVectorSyncPages; page++ {
		query.Set("page", strconv.Itoa(page))
		r, err := client.Do(ctx, token, "GET", "/repos/"+repo+"/issues?"+query.Encode(), nil)
		var list []qqHistoryIssue
		if err != nil || !r.OK() || r.Decode(&list) != nil {
			return synced, githubFailure("history", r, err)
		}
		ids := make([]string, 0, len(list))
		for _, issue := range list {
			ids = append(ids, repo+"#"+strconv.FormatInt(issue.Number, 10))
		}
		hashes, err := s.qqVectors.QQKnowledgeHashes(ctx, ids)
		if err != nil {
			return synced, err
		}
		var items []account.QQKnowledgeItem
		var texts []string
		latest := since
		for i, issue := range list {
			if issue.UpdatedAt.After(latest) {
				latest = issue.UpdatedAt
			}
			kind, state := "issue", qqIssueState(issue.State, issue.StateReason, issue.labelNames())
			if issue.PullRequest != nil {
				kind = "pr"
				switch merged, _, _ := strings.Cut(issue.PullRequest.MergedAt, "T"); {
				case merged != "":
					state = "已合并 " + merged
				case issue.State == "open":
					state = "开着"
				default:
					state = "已关闭未合并"
				}
			}
			body := qqPlainText(qqStripImagesAndHTML(issue.Body))
			h := qqContentHash(issue.Title, state, body, strconv.Itoa(issue.Comments))
			if kind == "issue" && issue.Comments > 0 {
				// 编辑已有评论不会改变评论数，仍需按 issue 的更新时间刷新维护者回复。
				h = qqContentHash(h, issue.UpdatedAt.UTC().Format(time.RFC3339Nano))
			}
			if hashes[ids[i]] == h {
				continue
			}
			// 维护者在 issue 下的回复常常就是答案；PR 的正文已经说明了改了什么，不再读评论。
			comment := ""
			if kind == "issue" {
				// issue 列表已更新，不能把短期缓存里的旧回复写成新版本并推进游标。
				client.Invalidate("/repos/" + repo + "/issues/" + strconv.FormatInt(issue.Number, 10) + "/comments")
				comment, err = s.qqLookupMaintainerComment(ctx, repo, issue.Number, issue.qqIssueDetail, qqHistorySummaryRunes)
				if err != nil {
					// 不写入这一页的摘要、哈希或游标，下一轮仍会读取这些 issue 并补全评论。
					return synced, err
				}
			}
			summary := state + "；" + qqTruncate(body, qqHistorySummaryRunes)
			if comment != "" {
				summary += "；维护者说：" + comment
			}
			items = append(items, account.QQKnowledgeItem{ID: ids[i], Kind: kind, Repo: short, Number: int(issue.Number), Title: issue.Title, Summary: summary, ContentHash: h, UpdatedAt: issue.UpdatedAt})
			texts = append(texts, qqTruncate(issue.Title+"\n"+body+"\n"+comment, qqEmbedRunes))
		}
		if err := s.qqEmbedItems(ctx, items, texts); err != nil {
			return synced, err
		}
		if err := s.qqVectors.UpsertQQKnowledge(ctx, items); err != nil {
			return synced, err
		}
		synced += len(items)
		// 整页写完才推进进度；GitHub 的 since 包含等于它的时间，边界上的条目下次会再读到一遍，摘要没变就跳过。
		if latest.After(since) {
			if err := s.qqVectors.SetQQKnowledgeCursor(ctx, repo, latest); err != nil {
				return synced, err
			}
		}
		if len(list) < 100 {
			break
		}
	}
	return synced, nil
}

// qqVectorRecall 是一次召回的结果：文档段落在 sections 里的下标（按相似度从高到低）和语义相近的 issue、PR。
type qqVectorRecall struct {
	docs    []int
	history []account.QQKnowledgeHit
}

// qqRecallDocSections 只为达到阈值的命中保存位置，避免每次召回都为全部段落建索引。
func qqRecallDocSections(sections []qqSection, hits []account.QQKnowledgeHit) []int {
	eligible := 0
	for _, hit := range hits {
		if hit.Similarity >= qqVectorMinSimilarity {
			eligible++
		}
	}
	if eligible == 0 || len(sections) == 0 {
		return nil
	}
	slots := make(map[string]int, eligible)
	positions := make([]int, 0, eligible)
	for _, hit := range hits {
		if hit.Similarity >= qqVectorMinSimilarity {
			if _, ok := slots[hit.ID]; !ok {
				slots[hit.ID] = len(positions)
				positions = append(positions, -1)
			}
		}
	}
	remaining := len(positions)
	// 旧索引保留同 ID 的最后一个段落；从末尾查找，全部命中找到后即可停止。
	for i := len(sections) - 1; i >= 0 && remaining > 0; i-- {
		id := qqDocIDBytes(sections[i])
		if slot, ok := slots[string(id[:])]; ok && positions[slot] < 0 {
			positions[slot] = i
			remaining--
		}
	}
	var docs []int
	for _, hit := range hits {
		if hit.Similarity >= qqVectorMinSimilarity {
			if position := positions[slots[hit.ID]]; position >= 0 {
				docs = append(docs, position)
			}
		}
	}
	return docs
}

// qqRecall 把问题算成向量并召回；功能关闭或任何一步失败时返回空结果，答疑照旧只用目录挑段落。
func (s *Server) qqRecall(ctx context.Context, question string, sections []qqSection, history bool) qqVectorRecall {
	var recall qqVectorRecall
	if s.qqVectors == nil || s.qqEmbeddingURL() == "" {
		return recall
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	vectors, err := s.qqEmbed(ctx, []string{qqTruncate(question, qqEmbedRunes)})
	if err != nil {
		slog.Warn("qq bot: question not embedded", "reason", err.Error())
		return recall
	}
	hits, err := s.qqVectors.SearchQQKnowledge(ctx, vectors[0], []string{"doc"}, qqVectorDocHits)
	if err != nil {
		slog.Warn("qq bot: vector search failed", "reason", err.Error())
		return recall
	}
	recall.docs = qqRecallDocSections(sections, hits)
	if !history {
		return recall
	}
	hits, err = s.qqVectors.SearchQQKnowledge(ctx, vectors[0], []string{"issue", "pr"}, qqVectorHistoryHits)
	if err != nil {
		slog.Warn("qq bot: vector search failed", "reason", err.Error())
		return recall
	}
	for _, hit := range hits {
		if hit.Similarity >= qqVectorMinSimilarity {
			recall.history = append(recall.history, hit)
		}
	}
	return recall
}

// qqHistoryMaterial 是交给回答模型的「语义相近的历史 issue 和 PR」。
func qqHistoryMaterial(hits []account.QQKnowledgeHit) string {
	var b strings.Builder
	for _, hit := range hits {
		kind := "issue"
		if hit.Kind == "pr" {
			kind = "PR"
		}
		fmt.Fprintf(&b, "- %s #%d（%s）%s：%s\n", hit.Repo, hit.Number, kind, hit.Title, hit.Summary)
	}
	return b.String()
}

// qqMergeSections 把向量召回的段落接在模型挑出的段落后面（去重），返回最终交给回答模型的段落下标。
func qqMergeSections(picked, recalled []int, limit int) []int {
	out := slices.Clone(picked)
	for _, i := range recalled {
		if len(out) >= limit {
			break
		}
		if !slices.Contains(out, i) {
			out = append(out, i)
		}
	}
	return out
}
