package account

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
)

// stageDictionarySnapshot 在修改用户数据前验证快照并写入暂存文件，替换来源词条 ID。
func stageDictionarySnapshot(ctx context.Context, reader io.Reader, writer io.Writer) error {
	entryCount := 0
	var buffer *bufio.Writer
	err := decodeDictionarySnapshot(reader, func(record snapshotRecord) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if record.Type == "entry" {
			entryCount++
			if entryCount > 100000 {
				return errDictionaryLimit
			}
			var e DictionaryEntry
			if err := json.Unmarshal(record.Data, &e); err != nil {
				return err
			}
			e.ID = randomToken()
			data, err := json.Marshal(e)
			record.Data = data
			if err != nil {
				return err
			}
		}
		// 头记录直接写出，空快照无需分配缓冲；数据记录合并写入，减少临时文件的小写入。
		if record.Type == "header" {
			return json.NewEncoder(writer).Encode(record)
		}
		if buffer == nil {
			buffer = bufio.NewWriter(writer)
		}
		return json.NewEncoder(buffer).Encode(record)
	})
	if err != nil {
		return err
	}
	if buffer != nil {
		// 引擎和 COPY 会重新读取暂存文件；全部数据落入文件且刷新成功后才能继续。
		return buffer.Flush()
	}
	return nil
}
