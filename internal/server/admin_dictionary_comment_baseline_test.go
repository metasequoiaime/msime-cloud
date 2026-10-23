package server

import (
	"encoding/csv"
	"strings"
)

// dictionaryRowsBeforeCommentSkip 保留先查询再过滤注释的路径，作为注释过滤优化的性能对照。
func dictionaryRowsBeforeCommentSkip(name, text, query string, page int) ([]dictionaryRow, int) {
	rows := []dictionaryRow{}
	total := 0
	q := strings.ToLower(query)
	yamlHeader := strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
	lineNumber := 0
	previousRaw := ""
	previousTrimmed := ""
	previousQueryLine := ""
	previousQueryMatch := false
	previousQueryReady := false
	for raw := range strings.SplitSeq(text, "\n") {
		lineNumber++
		line := previousTrimmed
		if raw != previousRaw {
			line = strings.TrimSpace(raw)
			previousRaw, previousTrimmed = raw, line
		}
		if yamlHeader {
			if line == "..." {
				yamlHeader = false
			}
			continue
		}
		queryMatch := true
		if q != "" {
			if previousQueryReady && line == previousQueryLine {
				queryMatch = previousQueryMatch
			} else {
				queryMatch = dictionaryLineContainsLower(line, q)
				previousQueryLine, previousQueryMatch, previousQueryReady = line, queryMatch, true
			}
		}
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "//") || !queryMatch {
			continue
		}
		total++
		if total <= (page-1)*100 || total > page*100 {
			continue
		}
		var cols []string
		if strings.HasSuffix(name, ".csv") {
			if !strings.ContainsRune(line, '"') {
				cols = strings.Split(line, ",")
			} else {
				reader := csv.NewReader(strings.NewReader(line))
				reader.FieldsPerRecord = -1
				if parsed, err := reader.Read(); err == nil {
					cols = parsed
				} else {
					cols = strings.Split(strings.TrimSuffix(raw, "\r"), "\t")
				}
			}
		} else if strings.HasPrefix(name, "sources/korean/") {
			cols = strings.SplitN(line, ":", 3)
		} else if !strings.HasSuffix(name, ".md") && !strings.ContainsRune(raw, '\t') {
			// 原始行的首尾制表符也代表列，不能用去除空白后的 line 判断。
			cols = strings.Fields(line)
		} else {
			cols = strings.Split(strings.TrimSuffix(raw, "\r"), "\t")
		}
		rows = append(rows, dictionaryRow{Line: lineNumber, Columns: cols})
	}
	return rows, total
}
