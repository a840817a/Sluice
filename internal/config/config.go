package config

import (
	"errors"
	"io"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Admin    AdminConfig    `yaml:"admin"`
	Upstream UpstreamConfig `yaml:"upstream"`
	Worker   WorkerConfig   `yaml:"worker"`
	Store    StoreConfig    `yaml:"store"`
	Window   WindowConfig   `yaml:"window"`
}

type AdminConfig struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type ServerConfig struct {
	Addr string `yaml:"addr"` // e.g. ":8080"
	// Gateway 對外 base URL，用於改寫 MPD 中的 segment/init/license URL
	BaseURL string    `yaml:"base_url"` // e.g. "https://gateway.example.com"
	TLS     TLSConfig `yaml:"tls"`
}

type TLSConfig struct {
	// CertFile / KeyFile：提供現有憑證時使用
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	// SelfSigned：啟動時自動產生自簽憑證（測試用）
	// IP 欄位指定要放入 SAN 的 IP 位址，例如 "1.2.3.4"
	SelfSigned bool   `yaml:"self_signed"`
	IP         string `yaml:"ip"`
}

type UpstreamConfig struct {
	// MPDURL 與 ForwardClientIP 都是 per-channel only：channel manager 會用
	// channel config 覆寫它們（見 channel.Manager.Start），所以放在 config.yaml
	// 沒有作用，標成 yaml:"-" 以免誤導。
	//
	// MPDURL 是上游來源 URL；HLS channel 放的是 playlist URL，欄位名沿用
	// 歷史名稱以維持既有 channels.json 相容。
	MPDURL string `yaml:"-"`
	// 是否透傳 X-Forwarded-For 到 license server。
	ForwardClientIP bool `yaml:"-"`
	// 上游認證 header，例如 "Authorization: Bearer <token>"
	AuthHeader string `yaml:"auth_header"`
	AuthValue  string `yaml:"auth_value"`
	// dynamic MPD 的 refresh 間隔（static MPD 忽略）
	PollInterval time.Duration `yaml:"poll_interval"`
}

type WorkerConfig struct {
	// 每個 channel 的 fetch worker 數量
	FetchWorkers int `yaml:"fetch_workers"`
	// 分段下載失敗後的最多重試次數。名稱沿用歷史，實際上 live 與 static
	// 兩種來源的 requeue 都套用這個上限。
	MaxRetriesStatic int `yaml:"max_retries_static"`
	// Reserved/deprecated: live fetches currently follow the upstream DVR
	// window so segments are retried while they are still advertised.
	LiveUsefulnessTTL time.Duration `yaml:"live_usefulness_ttl"`
	// 單一分段允許的最大位元組數。每個 worker 會把整個分段讀進記憶體，所以這是
	// 防止異常 origin 撐爆記憶體的上限，而不是調校參數；正常情況由回應自己的
	// Content-Length 決定。0 表示採用 fetch.DefaultMaxSegmentBytes（128 MiB）。
	// 若合法分段被擋下，log 會出現 "exceeds the size limit" 警告。
	MaxSegmentBytes int64 `yaml:"max_segment_bytes"`
	// 單一分段下載的逾時上限，涵蓋整個請求「包含讀完 body」，不是閒置逾時。
	// 也就是說一個持續在下載、只是比較慢的大分段，時間到照樣會被中斷，錯誤是
	// "context deadline exceeded ... while reading body"。
	//
	// 因此這個值要能容納「最大的分段 ÷ 最慢的可用頻寬」。高位元率來源配上較窄
	// 的上行頻寬時，預設的 30s 會不夠：20 Mbps 的 6 秒分段約 15 MB，低於 4 Mbps
	// 就必定超時。0 表示採用 fetch.DefaultSegmentTimeout（30s）。
	SegmentTimeout time.Duration `yaml:"segment_timeout"`
}

// CleanupMode controls when expired segment files are deleted from disk.
type CleanupMode string

const (
	CleanupDisabled CleanupMode = "disabled"  // never delete (default)
	CleanupOnExpire CleanupMode = "on_expire" // delete when segment slides out of window
)

type StoreConfig struct {
	// segment 儲存根目錄，結構：{data_dir}/{channel}/periods/{period}/{type}/{segNo}.m4s
	DataDir string      `yaml:"data_dir"`
	Cleanup CleanupMode `yaml:"cleanup"` // default: "disabled"
	// EnableVODTransition keeps all segments on disk during live and supports
	// automatic transition to a full-history VOD when the stream ends.
	// When true, Cleanup is ignored during live (no segments are deleted).
	EnableVODTransition bool `yaml:"enable_vod_transition"`
	// KeepAllSegments is per-channel only; populated by the channel manager from
	// the channel config. Keeps every fetched segment available in live DVR.
	KeepAllSegments bool `yaml:"-"`
}

type WindowConfig struct {
	// sliding window 深度（timeShiftBufferDepth）
	Depth time.Duration `yaml:"depth"`
	// live edge 後退量（safe edge buffer）
	SafeEdgeBuffer time.Duration `yaml:"safe_edge_buffer"`
}

func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	cfg := &Config{}
	// An empty file is a valid config, not a parse error: a mounted but empty
	// ConfigMap or a placeholder file means "override nothing", and the
	// environment and applyDefaults still have to run. The YAML decoder reports
	// an empty document as io.EOF, which is the only error that means this.
	if err := yaml.NewDecoder(f).Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	// YAML, then environment, then defaults. Defaults run last and only fill
	// values that are still empty, so an override is never clobbered.
	if err := cfg.applyEnv(); err != nil {
		return nil, err
	}
	cfg.applyDefaults()
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Addr == "" {
		c.Server.Addr = ":8080"
	}
	if c.Admin.Username == "" {
		c.Admin.Username = "admin"
	}
	if c.Admin.Password == "" {
		c.Admin.Password = DefaultAdminPassword
	}
	if c.Worker.FetchWorkers <= 0 {
		c.Worker.FetchWorkers = 4
	}
	if c.Worker.MaxRetriesStatic <= 0 {
		c.Worker.MaxRetriesStatic = 5
	}
	if c.Worker.LiveUsefulnessTTL <= 0 {
		c.Worker.LiveUsefulnessTTL = 30 * time.Second
	}
	if c.Worker.SegmentTimeout <= 0 {
		c.Worker.SegmentTimeout = 30 * time.Second
	}
	if c.Upstream.PollInterval <= 0 {
		c.Upstream.PollInterval = 2 * time.Second
	}
	if c.Window.Depth <= 0 {
		c.Window.Depth = 120 * time.Second
	}
	if c.Window.SafeEdgeBuffer <= 0 {
		c.Window.SafeEdgeBuffer = 6 * time.Second
	}
	if c.Store.DataDir == "" {
		c.Store.DataDir = "./data"
	}
}
