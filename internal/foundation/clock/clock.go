// Package clock 提供可注入的时间源。
//
// 运行预算、重试退避、租约判定全部走这里 —— 用墙钟会被校时影响，
// 而“用了多久”必须单调。生产实现使用 time.Now 的单调读数。
package clock

import (
	"context"
	"sync"
	"time"
)

type Clock interface {
	Now() time.Time
	Since(t time.Time) time.Duration
	Sleep(ctx context.Context, d time.Duration) error
}

type System struct{}

func (System) Now() time.Time                  { return time.Now() }
func (System) Since(t time.Time) time.Duration { return time.Since(t) }
func (System) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Fake 是确定性时钟，供单元测试推进时间。
type Fake struct {
	mu  sync.Mutex
	now time.Time
}

func NewFake(start time.Time) *Fake { return &Fake{now: start} }

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *Fake) Since(t time.Time) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now.Sub(t)
}

func (f *Fake) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.Advance(d)
	return nil
}

func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}
