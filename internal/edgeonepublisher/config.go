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
	defaultLeaseSeconds    = 600
	defaultUploadRateBPS   = 187_500 // 1.5 Mbps
	defaultMaxObjectBytes  = 23 << 20
	defaultHTTPTimeoutSecs = 600
)

type Config struct {
	CoreURL        string `json:"core_url"`
	PublisherToken string `json:"publisher_token"`
	SignerBaseURL  string `json:"signer_base_url"`
	SignerToken    string `json:"signer_token"`
	StatePath      string `json:"state_path"`
	Owner          string `json:"owner"`

	PollIntervalSeconds      int   `json:"poll_interval_seconds"`
	LeaseSeconds             int   `json:"lease_seconds"`
	UploadRateBytesPerSecond int64 `json:"upload_rate_bytes_per_second"`
	MaxObjectBytes           int64 `json:"max_object_bytes"`
	HTTPTimeoutSeconds       int   `json:"http_timeout_seconds"`
}

func DefaultConfig() Config {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "yuzu-edgeone"
	}
	return Config{
		StatePath:                "data/yuzu-edgeone.db",
		Owner:                    hostname,
		PollIntervalSeconds:      defaultPollSeconds,
		LeaseSeconds:             defaultLeaseSeconds,
		UploadRateBytesPerSecond: defaultUploadRateBPS,
		MaxObjectBytes:           defaultMaxObjectBytes,
		HTTPTimeoutSeconds:       defaultHTTPTimeoutSecs,
	}
}

func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	f, err := os.Open(path)
	if err != nil {
		return cfg, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
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
	for name, value := range map[string]string{
		"core_url":        c.CoreURL,
		"publisher_token": c.PublisherToken,
		"signer_base_url": c.SignerBaseURL,
		"signer_token":    c.SignerToken,
		"state_path":      c.StatePath,
		"owner":           c.Owner,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	for name, raw := range map[string]string{
		"core_url": c.CoreURL, "signer_base_url": c.SignerBaseURL,
	} {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("%s must be an absolute http(s) URL", name)
		}
	}
	if c.PollIntervalSeconds <= 0 || c.LeaseSeconds <= 0 || c.HTTPTimeoutSeconds <= 0 {
		return errors.New("poll_interval_seconds, lease_seconds and http_timeout_seconds must be positive")
	}
	if c.UploadRateBytesPerSecond < 0 {
		return errors.New("upload_rate_bytes_per_second must not be negative")
	}
	if c.MaxObjectBytes <= 0 {
		return errors.New("max_object_bytes must be positive")
	}
	return nil
}
