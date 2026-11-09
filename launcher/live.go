package main

import (
	"sync"
	"time"
)

// Live – telemetry mới nhất từ MCU (dòng W qua serial), giữ trong RAM, không ghi DB.
// Dùng cho tin báo khởi động (MCU online, trạng thái, cổng, cân hiện tại).
type Live struct {
	mu        sync.RWMutex
	kg        float64
	state     string
	gate      int // 1 = mở, 2 = đóng
	updatedAt time.Time
}

type liveSnap struct {
	Kg        float64
	State     string
	Gate      int
	McuOnline bool
}

func newLive() *Live { return &Live{state: "UNKNOWN", gate: 2} }

func (l *Live) update(kg float64, state string, gate int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.kg, l.state, l.gate, l.updatedAt = kg, state, gate, time.Now()
}

func (l *Live) snapshot() liveSnap {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return liveSnap{Kg: l.kg, State: l.state, Gate: l.gate,
		McuOnline: time.Since(l.updatedAt) < 3*time.Second}
}
