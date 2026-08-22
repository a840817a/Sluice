package channel

// Header represents a single HTTP header name/value pair.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Source protocols a channel can ingest from.
const (
	// SourceDASH ingests an upstream MPD. It is the historical behavior and the
	// meaning of an empty SourceType, so channels.json files written before
	// HLS support keep working unchanged.
	SourceDASH = "dash"
	// SourceHLS ingests an upstream HLS master or media playlist.
	SourceHLS = "hls"
)

// HLS AES-128 handling modes.
const (
	// KeyModePassthrough stores segments encrypted and proxies key requests.
	// It is the default, and the meaning of an empty HLSKeyMode.
	KeyModePassthrough = "passthrough"
	// KeyModeDecrypt decrypts segments during ingest and stores cleartext.
	KeyModeDecrypt = "decrypt"
)

// Config holds the per-channel configuration stored in channels.json.
type Config struct {
	Title string `json:"title,omitempty"`
	// SourceType selects the upstream protocol: "dash" (or empty) or "hls".
	SourceType string `json:"source_type,omitempty"`
	// HLSKeyMode selects how AES-128 encrypted HLS sources are handled:
	// "passthrough" (or empty) keeps segments encrypted on disk and rewrites
	// EXT-X-KEY to the gateway's key proxy; "decrypt" decrypts during ingest and
	// republishes in the clear.
	HLSKeyMode string `json:"hls_key_mode,omitempty"`
	// MPDURL is the upstream source URL. Despite the name it holds the HLS
	// playlist URL for HLS channels too; the field keeps its original name and
	// JSON tag so existing stored configs and the admin UI stay valid.
	MPDURL              string `json:"mpd_url"`
	PlayReadyLicenseURL string `json:"playready_license_url"`
	WidevineLicenseURL  string `json:"widevine_license_url"`
	// EnablePlayReady and EnableWidevine are pointers so older channel configs
	// without these fields keep the historical default: both DRM systems enabled.
	EnablePlayReady *bool    `json:"enable_playready,omitempty"`
	EnableWidevine  *bool    `json:"enable_widevine,omitempty"`
	FetchHeaders    []Header `json:"fetch_headers,omitempty"`   // injected into upstream MPD/segment requests
	LicenseHeaders  []Header `json:"license_headers,omitempty"` // injected into DRM license proxy requests
	ForwardClientIP bool     `json:"forward_client_ip"`
	Enabled         bool     `json:"enabled"`
	// EnableVODTransition keeps all downloaded segments on disk during live
	// and automatically transitions to full-history VOD when the stream ends
	// (upstream MPD changes from type=dynamic to type=static, or via admin API).
	EnableVODTransition bool `json:"enable_vod_transition"`
	// IsVOD is set to true after a successful live→VOD transition so the gateway
	// can restore VOD mode (using an unbounded ring buffer) after a restart.
	IsVOD bool `json:"is_vod,omitempty"`
	// KeepAllSegments keeps every fetched segment available in the live DVR
	// window for the lifetime of the channel (no time-based expiration,
	// unbounded ring buffer). Distinct from EnableVODTransition: this stays
	// in live mode even when upstream goes static.
	KeepAllSegments bool `json:"keep_all_segments,omitempty"`
}

// BoolPtr returns a pointer to b for optional JSON booleans.
func BoolPtr(b bool) *bool {
	return &b
}

// HeaderMap flattens a header list into the map form the fetch, watcher and key
// packages apply to outbound requests. Nameless entries are dropped: the admin
// UI submits a blank row for every unfilled slot in the form.
func HeaderMap(hs []Header) map[string]string {
	m := make(map[string]string, len(hs))
	for _, h := range hs {
		if h.Name != "" {
			m[h.Name] = h.Value
		}
	}
	return m
}

// IsHLS reports whether this channel ingests from an HLS source.
func (c Config) IsHLS() bool { return c.SourceType == SourceHLS }

// canonicalSourceType resolves the empty value to its documented default, so a
// config stored before HLS support compares equal to one that names "dash"
// explicitly. The admin UI always submits an explicit value, which would
// otherwise read as an edit — and cost a needless restart — on the first save
// of any older channel.
func (c Config) canonicalSourceType() string {
	if c.SourceType == "" {
		return SourceDASH
	}
	return c.SourceType
}

// canonicalHLSKeyMode resolves the empty value to its documented default, for
// the same reason as canonicalSourceType.
func (c Config) canonicalHLSKeyMode() string {
	if c.HLSKeyMode == "" {
		return KeyModePassthrough
	}
	return c.HLSKeyMode
}

// DecryptOnIngest reports whether encrypted HLS segments should be decrypted
// before being stored. Defaults to false (passthrough) so segments keep their
// upstream protection unless the operator opts out of it.
func (c Config) DecryptOnIngest() bool { return c.HLSKeyMode == KeyModeDecrypt }

// PlayReadyEnabled returns true unless the channel explicitly disables
// PlayReady. This preserves behavior for configs created before the flag
// existed.
func (c Config) PlayReadyEnabled() bool {
	return c.EnablePlayReady == nil || *c.EnablePlayReady
}

// WidevineEnabled returns true unless the channel explicitly disables Widevine.
func (c Config) WidevineEnabled() bool {
	return c.EnableWidevine == nil || *c.EnableWidevine
}
