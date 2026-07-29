package distribution

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

type HealthMonitor struct {
	st       *store.Store
	client   *http.Client
	interval time.Duration
	now      func() time.Time
}

func NewHealthMonitor(st *store.Store) *HealthMonitor {
	return &HealthMonitor{
		st: st, client: &http.Client{Timeout: 10 * time.Second},
		interval: time.Minute, now: time.Now,
	}
}

func (m *HealthMonitor) Run(ctx context.Context) {
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

func (m *HealthMonitor) checkAll(ctx context.Context) {
	accelerations, err := m.st.ListAccelerations(ctx)
	if err != nil {
		return
	}
	for _, acceleration := range accelerations {
		controlOK, backendOK, detail := CheckHealth(ctx, m.client, acceleration, acceleration.BackendToken)
		_ = m.st.UpdateAccelerationHealth(ctx, acceleration.ID, controlOK, backendOK,
			detail, m.now().UnixMilli())
	}
}

func CheckHealth(
	ctx context.Context,
	client *http.Client,
	acceleration store.Acceleration,
	backendToken string,
) (bool, bool, string) {
	controlOK, controlDetail := probeHealth(ctx, client, acceleration.ControlBaseURL+"/health", "")
	backendOK, backendDetail := probeHealth(ctx, client, acceleration.BackendBaseURL+"/health", backendToken)
	details := make([]string, 0, 2)
	if !controlOK {
		details = append(details, "control: "+controlDetail)
	}
	if !backendOK {
		details = append(details, "backend: "+backendDetail)
	}
	return controlOK, backendOK, strings.Join(details, "; ")
}

func probeHealth(ctx context.Context, client *http.Client, endpoint, token string) (bool, string) {
	if endpoint == "/health" {
		return false, "endpoint not configured"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, err.Error()
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	if err != nil {
		return false, err.Error()
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return false, fmt.Sprintf("status %d", response.StatusCode)
	}
	return true, ""
}
