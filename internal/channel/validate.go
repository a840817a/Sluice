package channel

import (
	"errors"
	"fmt"
	"net/url"

	"github.com/a840817a/sluice/internal/cenc"
)

// dashDecryptEnabled gates DASH channels in decrypt mode until the ingest
// pipeline can decrypt them. Until then such a channel would store ciphertext
// and claim it was cleartext.
var dashDecryptEnabled = false

// Validate runs the checks every write path applies to a channel config.
//
// These are domain invariants, not HTTP concerns: what counts as a usable
// source type, key mode, or source URL is a property of a channel, and the
// admin API is only one caller. The error strings are nevertheless part of that
// API's response contract — clients key on the "mpd_url: " prefix — so they are
// reproduced verbatim rather than reworded.
func (c Config) Validate() error {
	if err := c.ValidateUpdate(); err != nil {
		return err
	}
	if c.canonicalSourceType() == SourceDASH && c.canonicalKeyMode() == KeyModeDecrypt && !dashDecryptEnabled {
		return errors.New("DASH decrypt is not supported yet")
	}
	return c.validateContentKeys()
}

// ValidateUpdate is Validate without the content key rules. An edit never
// carries key material (the stored keys are kept), so those rules apply to
// creation and to the stored config, not to the request.
func (c Config) ValidateUpdate() error {
	if err := validateSourceURL(c.MPDURL); err != nil {
		return fmt.Errorf("mpd_url: %w", err)
	}
	if err := validateSourceType(c.SourceType); err != nil {
		return err
	}
	return validateKeyMode(c.KeyMode)
}

// validateContentKeys checks how key_mode, source_type and content_keys fit
// together: keys belong to DASH channels in decrypt mode, which need at least
// one, and each KID may appear once.
func (c Config) validateContentKeys() error {
	dashDecrypt := c.canonicalSourceType() == SourceDASH && c.canonicalKeyMode() == KeyModeDecrypt
	if !dashDecrypt {
		if len(c.ContentKeys) > 0 {
			return errors.New("content_keys are only allowed for a dash source with key_mode decrypt")
		}
		return nil
	}
	if len(c.ContentKeys) == 0 {
		return errors.New("content_keys: a dash source with key_mode decrypt needs at least one key")
	}
	_, err := parseContentKeys(c.ContentKeys)
	return err
}

// parseContentKeys parses "kid:key" strings, rejecting a KID given twice. The
// errors never contain key digits.
func parseContentKeys(keys []string) ([]cenc.Key, error) {
	out := make([]cenc.Key, 0, len(keys))
	seen := make(map[cenc.KID]bool, len(keys))
	for i, s := range keys {
		k, err := cenc.ParseKey(s)
		if err != nil {
			return nil, fmt.Errorf("content_keys[%d]: %w", i, err)
		}
		if seen[k.KID] {
			return nil, fmt.Errorf("content_keys[%d]: duplicate KID %s", i, k.KID)
		}
		seen[k.KID] = true
		out = append(out, k)
	}
	return out, nil
}

// validateSourceType ensures the channel's source protocol is one we support.
// An empty value keeps the historical DASH default so configs written before
// HLS support remain valid.
func validateSourceType(s string) error {
	switch s {
	case "", SourceDASH, SourceHLS:
		return nil
	}
	return fmt.Errorf("source_type must be %q or %q", SourceDASH, SourceHLS)
}

// validateKeyMode ensures the key mode is one we support. Empty
// means passthrough, which is the safe default: it keeps origin protection.
func validateKeyMode(s string) error {
	switch s {
	case "", KeyModePassthrough, KeyModeDecrypt:
		return nil
	}
	return fmt.Errorf("key_mode must be %q or %q", KeyModePassthrough, KeyModeDecrypt)
}

// validateSourceURL ensures the URL is well-formed and uses http or https only.
// This prevents SSRF via file://, ftp://, or internal metadata endpoints.
// The field carries the HLS playlist URL for HLS channels too.
func validateSourceURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("mpd_url must use http or https scheme")
	}
	if u.Host == "" {
		return fmt.Errorf("mpd_url must include a host")
	}
	return nil
}
