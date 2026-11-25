package sim

import "math/rand"

// newTestRNG 测试用固定随机源,与模拟器内部随机源隔离。
func newTestRNG() *rand.Rand { return rand.New(rand.NewSource(42)) }
