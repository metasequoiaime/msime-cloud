package server

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

type dictionarySearchResponse struct {
	Files      []dictionarySearchFile `json:"files"`
	Skipped    []map[string]string    `json:"skipped"`
	NextCursor int                    `json:"next_cursor"`
	TotalFiles int                    `json:"total_files"`
	HasMore    bool                   `json:"has_more"`
	Revision   string                 `json:"revision"`
	Truncated  bool                   `json:"truncated"`
}

func TestDictionarySearchAcrossFilesAndRevisions(t *testing.T) {
	f := &dictionaryMock{text: "# 说明\n甲\tjia\t1\n乙\tyi\t5\n"}
	s := &Server{adminGitHub: dictionaryMockClient(t, f), config: Config{Admin: AdminConfig{GitHub: AdminGitHubConfig{DictionaryRepo: dictTestRepo}}}}
	w := dictionaryDirect(s, "GET", "/api/dictionary/search?q=JIA", "", false)
	result := decodeDict[dictionarySearchResponse](t, w)
	if w.Code != 200 || len(result.Files) != 3 || result.TotalFiles != 3 || result.HasMore || result.NextCursor != 3 || result.Revision != strings.Repeat("c", 40) {
		t.Fatalf("status=%d result=%+v", w.Code, result)
	}
	for _, file := range result.Files {
		if file.Total != 1 || len(file.Rows) != 1 || file.Rows[0].Line != 2 || file.Rows[0].Columns[0] != "甲" {
			t.Fatalf("file=%+v", file)
		}
		if file.Editable == strings.HasPrefix(file.Path, "sources/") {
			t.Fatalf("editable=%+v", file)
		}
	}
	for _, scope := range []string{"custom", "packs", "sources"} {
		w = dictionaryDirect(s, "GET", "/api/dictionary/search?q=甲&scope="+scope, "", false)
		result = decodeDict[dictionarySearchResponse](t, w)
		if w.Code != 200 || result.TotalFiles != 1 || len(result.Files) != 1 || !strings.HasPrefix(result.Files[0].Path, scope+"/") {
			t.Fatalf("scope=%s result=%+v", scope, result)
		}
	}
	w = dictionaryDirect(s, "GET", "/api/dictionary/search?q=不存在", "", false)
	result = decodeDict[dictionarySearchResponse](t, w)
	if w.Code != 200 || len(result.Files) != 0 || result.TotalFiles != 3 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestDictionarySearchBatchesAndSkippedFiles(t *testing.T) {
	f := &dictionaryMock{blobs: map[string]string{}, truncated: true}
	for i := 0; i < 7; i++ {
		sha := fmt.Sprintf("%040x", i+1)
		f.tree = append(f.tree, dictionaryTreeItem{Path: fmt.Sprintf("sources/demo/%d.txt", i), Type: "blob", SHA: sha, Size: 40})
		f.blobs[sha] = "词条\tci'tiao\t1\n"
	}
	f.tree[1].Size = (64 << 20) + 1
	f.blobs[f.tree[2].SHA] = string([]byte{0x80, 0x81})
	f.blobs[f.tree[4].SHA] = strings.Repeat("词条\tci'tiao\t1\n", 105)
	s := &Server{adminGitHub: dictionaryMockClient(t, f), config: Config{Admin: AdminConfig{GitHub: AdminGitHubConfig{DictionaryRepo: dictTestRepo}}}}
	w := dictionaryDirect(s, "GET", "/api/dictionary/search?q=词条", "", false)
	first := decodeDict[dictionarySearchResponse](t, w)
	if w.Code != 200 || first.NextCursor != 4 || !first.HasMore || len(first.Files) != 2 || len(first.Skipped) != 2 || !first.Truncated || first.TotalFiles != 7 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = dictionaryDirect(s, "GET", "/api/dictionary/search?q=词条&cursor=4&revision="+first.Revision, "", false)
	second := decodeDict[dictionarySearchResponse](t, w)
	if w.Code != 200 || second.NextCursor != 7 || second.HasMore || len(second.Files) != 3 || second.Files[0].Total != 105 || len(second.Files[0].Rows) != 100 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = dictionaryDirect(s, "GET", "/api/dictionary/search?q=词条&cursor=7&revision="+first.Revision, "", false)
	if w.Code != 200 || decodeDict[dictionarySearchResponse](t, w).HasMore {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, request := range f.requests {
		if request.path == "/repos/"+dictTestRepo+"/git/blobs/"+f.tree[1].SHA {
			t.Fatal("超限文件不应读取")
		}
	}
}

func TestDictionarySearchRejectsInvalidQueriesAndUpstreamFailures(t *testing.T) {
	f := &dictionaryMock{text: "甲\tjia\t1\n"}
	s := &Server{adminGitHub: dictionaryMockClient(t, f), config: Config{Admin: AdminConfig{GitHub: AdminGitHubConfig{DictionaryRepo: dictTestRepo}}}}
	for _, query := range []string{"", "q=%0A", "q=%FF", "q=a&scope=bad", "q=a&cursor=-1", "q=a&cursor=bad", "q=a&cursor=1", "q=a&revision=bad", "q=" + url.QueryEscape(strings.Repeat("词", 100)), "q=a&cursor=999&revision=" + strings.Repeat("c", 40)} {
		w := dictionaryDirect(s, "GET", "/api/dictionary/search?"+query, "", false)
		if w.Code != 400 {
			t.Fatalf("query=%q status=%d", query, w.Code)
		}
	}
	root := "/repos/" + dictTestRepo
	for _, failure := range []string{root, root + "/git/trees/main", root + "/git/blobs/" + dictionaryBlobSHA([]byte(f.text))} {
		f := &dictionaryMock{text: "甲\tjia\t1\n", failMethod: "GET", failPath: failure, failStatus: 503}
		s := &Server{adminGitHub: dictionaryMockClient(t, f), config: s.config}
		w := dictionaryDirect(s, "GET", "/api/dictionary/search?q=甲", "", false)
		if w.Code != 503 || strings.Contains(w.Body.String(), "private upstream") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestDictionarySearchBoundsBatchBytes(t *testing.T) {
	f := &dictionaryMock{blobs: map[string]string{}}
	for i, size := range []int64{5 << 20, 5 << 20, 36 << 20, 100} {
		sha := fmt.Sprintf("%040x", i+1)
		f.tree = append(f.tree, dictionaryTreeItem{Path: fmt.Sprintf("sources/demo/%d.txt", i), Type: "blob", SHA: sha, Size: size})
		f.blobs[sha] = "词条\tci'tiao\t1\n"
	}
	s := &Server{adminGitHub: dictionaryMockClient(t, f), config: Config{Admin: AdminConfig{GitHub: AdminGitHubConfig{DictionaryRepo: dictTestRepo}}}}
	for cursor := 0; cursor < 4; cursor++ {
		query := fmt.Sprintf("/api/dictionary/search?q=词条&cursor=%d&revision=%s", cursor, strings.Repeat("c", 40))
		w := dictionaryDirect(s, "GET", query, "", false)
		result := decodeDict[dictionarySearchResponse](t, w)
		if w.Code != 200 || result.NextCursor != cursor+1 || len(result.Files) != 1 || result.HasMore != (cursor < 3) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestDictionarySearchNeverReadsDocumentationOrEngineTables(t *testing.T) {
	f := &dictionaryMock{blobs: map[string]string{}}
	paths := []string{"packs/README.md", "packs/demo/README.md", "sources/japanese/README.txt", "sources/japanese/LICENSE", "sources/japanese/connection_single_column.txt", "sources/japanese/id.def", "sources/japanese/dictionary_filter.tsv", "sources/pinyin/config.json", "sources/japanese/dictionary00.txt"}
	for i, name := range paths {
		sha := fmt.Sprintf("%040x", i+1)
		f.tree = append(f.tree, dictionaryTreeItem{Path: name, Type: "blob", SHA: sha, Size: 20})
		if i == len(paths)-1 {
			f.blobs[sha] = "れい\t1\t2\t300\t例\n"
		}
	}
	s := &Server{adminGitHub: dictionaryMockClient(t, f), config: Config{Admin: AdminConfig{GitHub: AdminGitHubConfig{DictionaryRepo: dictTestRepo}}}}
	w := dictionaryDirect(s, "GET", "/api/dictionary/search?q=例", "", false)
	result := decodeDict[dictionarySearchResponse](t, w)
	if w.Code != 200 || result.TotalFiles != 1 || len(result.Files) != 1 || result.Files[0].Path != paths[len(paths)-1] || result.Files[0].Rows[0].Columns[4] != "例" {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, request := range f.requests {
		if strings.Contains(request.path, "/git/blobs/") && !strings.HasSuffix(request.path, f.tree[len(paths)-1].SHA) {
			t.Fatalf("不应读取非词条文件: %s", request.path)
		}
	}
}
