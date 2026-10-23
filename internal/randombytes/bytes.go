// Package randombytes 提供按指定长度生成的安全随机字节。
package randombytes

import "crypto/rand"

// New 返回 size 字节的安全随机数，每次调用分配独立的缓冲区；size 必须非负。
func New(size int) []byte {
	b := make([]byte, size)
	// crypto/rand.Read 保证填满缓冲区且不返回错误，随机源故障时由标准库终止进程。
	_, _ = rand.Read(b)
	return b
}
