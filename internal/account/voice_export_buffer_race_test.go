//go:build race

package account

// 竞态检测会随机丢弃 sync.Pool 中的编码缓冲；为该模式的冷缓冲保留实测余量。
const voiceExportBufferAllocationBudget = 14 << 20
