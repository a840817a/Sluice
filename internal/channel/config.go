package channel

import (
	"fmt"

	"github.com/a840817a/sluice/internal/cenc"
)

// Header represents a single HTTP header name/value pair.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Source protocols a channel can ingest from.
const (
	// SourceDASH ingests an origin MPD. It is the historical behavior and the
	// meaning of an empty SourceType, so channels.json files written before
	// HLS support keep working unchanged.
	SourceDASH = "dash"
	// SourceHLS ingests an origin HLS master or media playlist.
	SourceHLS = "hls"
)

// Key modes: what the gateway does with encrypted content.
const (
	// KeyModePassthrough stores segments encrypted. For HLS AES-128 it also
	// proxies key requests; for DASH the license proxy serves the players. It
	// is the default, and the meaning of an empty KeyMode.
	KeyModePassthrough = "passthrough"
	// KeyModeDecrypt decrypts segments during ingest and stores cleartext.
	KeyModeDecrypt = "decrypt"
)

// Config holds the per-channel configuration stored in channels.json.
//
// Its JSON tags are also the admin API's request and response format, so a
// renamed or retagged field breaks both stored configs and API clients.
type Config struct {
	Title string `json:"title,omitempty"`
	// SourceType selects the source protocol: "dash" (or empty) or "hls".
	SourceType string `json:"source_type,omitempty"`
	// KeyMode selects how encrypted sources are handled: "passthrough" (or
	// empty) keeps segments encrypted on disk (for HLS AES-128 it rewrites
	// EXT-X-KEY to the gateway's key proxy); "decrypt" decrypts during ingest and
	// republishes in the clear. It is set when the channel is created and never
	// changes afterwards.
	KeyMode string `json:"key_mode,omitempty"`
	// LegacyHLSKeyMode is the old JSON name of KeyMode, still accepted when
	// loading channels.json and in admin requests. NormalizeKeyAlias folds it
	// into KeyMode and empties it, so nothing is ever written under the old name.
	LegacyHLSKeyMode string `json:"hls_key_mode,omitempty"`
	// ContentKeys are "kid:key" pairs (32 hex digits each) for decrypting a DASH
	// source. Only DASH channels in decrypt mode carry them. They are secrets:
	// responses show ContentKeyIDs instead (see PublicConfig).
	ContentKeys []string `json:"content_keys,omitempty"`
	// MPDURL is the source URL. Despite the name it holds the HLS
	// playlist URL for HLS channels too; the field keeps its original name and
	// JSON tag so existing stored configs and the admin UI stay valid.
	MPDURL              string `json:"mpd_url"`
	PlayReadyLicenseURL string `json:"playready_license_url"`
	WidevineLicenseURL  string `json:"widevine_license_url"`
	// EnablePlayReady and EnableWidevine are pointers so older channel configs
	// without these fields keep the historical default: both DRM systems enabled.
	EnablePlayReady *bool    `json:"enable_playready,omitempty"`
	EnableWidevine  *bool    `json:"enable_widevine,omitempty"`
	FetchHeaders    []Header `json:"fetch_headers,omitempty"`   // injected into origin MPD/segment requests
	LicenseHeaders  []Header `json:"license_headers,omitempty"` // injected into DRM license proxy requests
	ForwardClientIP bool     `json:"forward_client_ip"`
	Enabled         bool     `json:"enabled"`
	// IsVOD is set to true after a successful live→VOD transition so the gateway
	// can restore VOD mode (using an unbounded ring buffer) after a restart.
	IsVOD bool `json:"is_vod,omitempty"`
	// KeepAllSegments keeps every fetched segment available in the live DVR
	// window for the lifetime of the channel: no time-based expiration, and an
	// unbounded ring buffer. It is a pointer because nil — the value a config
	// stored before this became the default decodes to — means "keep", not
	// "window": only an explicit false selects the sliding window.
	//
	// It does not decide whether the channel transitions to VOD when the
	// origin ends; that happens either way. It decides how much history the
	// live channel still holds when it does.
	KeepAllSegments *bool `json:"keep_all_segments,omitempty"`
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

// canonicalKeyMode resolves the empty value to its documented default, for
// the same reason as canonicalSourceType.
func (c Config) canonicalKeyMode() string {
	if c.KeyMode == "" {
		return KeyModePassthrough
	}
	return c.KeyMode
}

// NormalizeKeyAlias folds the legacy "hls_key_mode" name into KeyMode. Both
// names given with different values is an error. It is a method rather than a
// custom UnmarshalJSON because the admin's create request embeds Config, and a
// promoted UnmarshalJSON would swallow the request's own fields.
func (c *Config) NormalizeKeyAlias() error {
	if c.LegacyHLSKeyMode == "" {
		return nil
	}
	if c.KeyMode != "" && c.KeyMode != c.LegacyHLSKeyMode {
		return fmt.Errorf("key_mode %q conflicts with hls_key_mode %q", c.KeyMode, c.LegacyHLSKeyMode)
	}
	c.KeyMode = c.LegacyHLSKeyMode
	c.LegacyHLSKeyMode = ""
	return nil
}

// DecryptOnIngest reports whether encrypted segments should be decrypted
// before being stored. Defaults to false (passthrough) so segments keep their
// origin protection unless the operator opts out of it.
func (c Config) DecryptOnIngest() bool { return c.KeyMode == KeyModeDecrypt }

// IsDASHDecrypt reports whether c is a DASH source in decrypt mode, the only
// kind of channel that takes content keys.
func (c Config) IsDASHDecrypt() bool {
	return c.canonicalSourceType() == SourceDASH && c.canonicalKeyMode() == KeyModeDecrypt
}

// ContentKeyIDs lists the KIDs (lower-case hex) of the stored content keys, in
// stored order. Entries that do not parse are skipped.
func (c Config) ContentKeyIDs() []string {
	var out []string
	for _, s := range c.ContentKeys {
		if k, err := cenc.ParseKey(s); err == nil {
			out = append(out, k.KID.String())
		}
	}
	return out
}

// PublicConfig is a Config as the admin API shows it: content key values are
// replaced by their KIDs.
type PublicConfig struct {
	Config
	ContentKeyIDs []string `json:"content_key_ids,omitempty"`
}

// Public returns c with its key values removed.
func (c Config) Public() PublicConfig {
	ids := c.ContentKeyIDs()
	c = cloneConfig(c)
	c.ContentKeys = nil
	return PublicConfig{Config: c, ContentKeyIDs: ids}
}

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

// KeepsAllSegments returns true unless the channel explicitly asks for the
// sliding window. Keeping everything is the default: a channel that ends up as
// VOD is only as complete as what it still holds, and an operator who wants the
// window has to say so.
func (c Config) KeepsAllSegments() bool {
	return c.KeepAllSegments == nil || *c.KeepAllSegments
}
