//go:build !race

package account

const voiceExportBufferAllocationBudget = 6 << 20
