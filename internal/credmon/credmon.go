// Package credmon 凭据健康检查：定期对所有 CredentialAware provider
// 探活，状态写回 credentials 表，翻转时记审计日志并告警。
//
// 只做"可见、可告警"——不做自动重登（扫码必须人来）。
package credmon

import (
	"context"
	"log"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

const DefaultInterval = 10 * time.Minute

type Monitor struct {
	reg      *provider.Registry
	st       *store.Store
	interval time.Duration

	last map[string]string // provider -> 上次检查状态（内存态，翻转检测用）
}

func New(reg *provider.Registry, st *store.Store) *Monitor {
	return &Monitor{reg: reg, st: st, interval: DefaultInterval, last: map[string]string{}}
}

// SetInterval 覆盖检查周期（测试用）。
func (m *Monitor) SetInterval(d time.Duration) { m.interval = d }

// Run 主循环：启动即查一次，此后按周期检查，直到 ctx 取消。
func (m *Monitor) Run(ctx context.Context) {
	m.checkAll(ctx)
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkAll(ctx)
		}
	}
}

func (m *Monitor) checkAll(ctx context.Context) {
	for _, p := range m.reg.All() {
		ca, ok := p.(provider.CredentialAware)
		if !ok {
			continue
		}
		m.checkOne(ctx, ca)
	}
}

func (m *Monitor) checkOne(ctx context.Context, ca provider.CredentialAware) {
	id := ca.ID()
	// 无凭据的 provider 跳过（unset 不需要探活）
	if cur, err := m.st.GetCredentialStatus(ctx, id); err != nil || cur == "unset" {
		return
	}

	checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	status := ca.CredentialStatus(checkCtx)
	cancel()

	if err := m.st.UpdateCredentialStatus(ctx, id, status); err != nil {
		log.Printf("[credmon] %s: update status: %v", id, err)
		return
	}

	prev, seen := m.last[id]
	m.last[id] = status
	switch {
	case !seen:
		log.Printf("[credmon] %s: credential status = %s", id, status)
	case prev != status:
		log.Printf("[credmon] WARNING: %s credential %s -> %s", id, prev, status)
		_ = m.st.Audit(ctx, "system", "credential.status_change", id,
			`{"from":"`+prev+`","to":"`+status+`"}`)
	}
}
