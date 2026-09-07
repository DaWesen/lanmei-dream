package ai

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestCompressorLockUserSerializes 验证同一用户的压缩锁串行化：
// 并发调用 lockUser 时任意时刻只有一个持有者（互斥），且全部调用都能完成（无死锁/丢失）。
func TestCompressorLockUserSerializes(t *testing.T) {
	c := &Compressor{}

	const workers = 20
	var (
		wg            sync.WaitGroup
		cur           atomic.Int32 // 当前持锁数量
		maxConcurrent atomic.Int32 // 观测到的最大并发持锁数
		completed     atomic.Int32 // 完成数
	)

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			unlock := c.lockUser(12345)
			n := cur.Add(1)
			// 更新观测到的最大并发
			for {
				m := maxConcurrent.Load()
				if n <= m || maxConcurrent.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			cur.Add(-1)
			unlock()
			completed.Add(1)
		}()
	}
	wg.Wait()

	if completed.Load() != workers {
		t.Fatalf("部分锁调用未完成: completed=%d workers=%d", completed.Load(), workers)
	}
	if maxConcurrent.Load() != 1 {
		t.Fatalf("同一用户锁未串行化: 观测到最大并发持锁数=%d，预期 1", maxConcurrent.Load())
	}
}

// TestCompressorLockUserDifferentUsers 验证不同用户互不阻塞：
// 两个用户并发持锁应能同时进入（不互相等待）。
func TestCompressorLockUserDifferentUsers(t *testing.T) {
	c := &Compressor{}

	var (
		wg            sync.WaitGroup
		cur           atomic.Int32
		maxConcurrent atomic.Int32
	)
	wg.Add(2)
	for _, uid := range []int64{1, 2} {
		go func(uid int64) {
			defer wg.Done()
			unlock := c.lockUser(uid)
			n := cur.Add(1)
			for {
				m := maxConcurrent.Load()
				if n <= m || maxConcurrent.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			cur.Add(-1)
			unlock()
		}(uid)
	}
	wg.Wait()

	if maxConcurrent.Load() != 2 {
		t.Fatalf("不同用户被错误串行化: 最大并发持锁数=%d，预期 2", maxConcurrent.Load())
	}
}
