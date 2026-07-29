package edgeonepublisher

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

const (
	defaultPollSeconds     = 2
	defaultHTTPTimeoutSecs = 600
)

// Config contains only local bootstrap state. Acceleration endpoints and
// publishing policy are loaded from Yuzu Core using PublisherToken.
type Config struct {
	CoreURL        string `json:"core_url"`
	PublisherToken string `json:"publisher_token"`
	StatePath      string `json:"state_path"`
	Owner          string `json:"owner"`

	PollIntervalSeconds int `json:"poll_interval_seconds"`
	HTTPTimeoutSeconds  int `json:"http_timeout_seconds"`
}

type ManagedConfig struct {
	AccelerationID              string `json:"acceleration_id"`
	Enabled                     bool   `json:"enabled"`
	Kind                        string `json:"kind"`
	BackendBaseURL              string `json:"backend_base_url"`
	BackendToken                string `json:"backend_token"`
	LeaseTTLSeconds             int    `json:"lease_ttl_seconds"`
	UploadRateBytesPerSecond    int64  `json:"upload_rate_bytes_per_second"`
	MaxObjectBytes              int64  `json:"max_object_bytes"`
	StorageBudgetBytes          int64  `json:"storage_budget_bytes"`
	StorageHighWatermarkPercent int    `json:"storage_high_watermark_percent"`
	StorageLowWatermarkPercent  int    `json:"storage_low_watermark_percent"`
}

func DefaultConfig() Config {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "yuzu-edgeone"
	}
	return Config{
		CoreURL: "http://127.0.0.1:8080", StatePath: "data/yuzu-edgeone.db",
		Owner: hostname, PollIntervalSeconds: defaultPollSeconds,
		HTTPTimeoutSeconds: defaultHTTPTimeoutSecs,
	}
}

func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	file, err := os.Open(path)
	if err != nil {
		return cfg, fmt.Errorf("open config: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.PublisherToken) == "" {
		return errors.New("publisher_token is required")
	}
	if strings.TrimSpace(c.Owner) == "" {
		return errors.New("owner is required")
	}
	if strings.TrimSpace(c.StatePath) == "" {
		return errors.New("state_path is required")
	}
	parsed, err := url.Parse(c.CoreURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return errors.New("core_url must be an absolute URL")
	}
	if c.PollIntervalSeconds <= 0 {
		return errors.New("poll_interval_seconds must be positive")
	}
	if c.HTTPTimeoutSeconds <= 0 {
		return errors.New("http_timeout_seconds must be positive")
	}
	return nil
}

func (c ManagedConfig) Validate() error {
	if c.AccelerationID == "" || c.Kind != "edgeone" || c.BackendToken == "" {
		return errors.New("managed acceleration configuration is incomplete")
	}
	parsed, err := url.Parse(c.BackendBaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return errors.New("managed backend_base_url must be an absolute URL")
	}
	if c.LeaseTTLSeconds <= 0 || c.UploadRateBytesPerSecond < 0 || c.MaxObjectBytes <= 0 ||
		c.StorageBudgetBytes <= 0 || c.StorageLowWatermarkPercent <= 0 ||
		c.StorageHighWatermarkPercent > 100 ||
		c.StorageLowWatermarkPercent >= c.StorageHighWatermarkPercent {
		return errors.New("managed acceleration limits are invalid")
	}
	return nil
}
