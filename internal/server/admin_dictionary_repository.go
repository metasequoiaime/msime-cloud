package server

import (
	"net/http"

	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

// dictionaryRepositoryMetadata 共用只读令牌与仓库查询；各入口继续解码自己使用的字段。
func dictionaryRepositoryMetadata(w http.ResponseWriter, r *http.Request, gh *githubapp.Client, repo string) (string, githubapp.Response, bool) {
	token, err := gh.Token(r.Context(), repo, dictionaryReadPermissions)
	if err != nil {
		dictGitHubError(w, err)
		return "", githubapp.Response{}, false
	}
	meta, err := gh.Get(r.Context(), token, "/repos/"+repo)
	if err != nil {
		dictGitHubError(w, err)
		return "", githubapp.Response{}, false
	}
	if !meta.OK() {
		dictGitHubStatus(w, "repository metadata", meta.Status)
		return "", githubapp.Response{}, false
	}
	return token, meta, true
}
