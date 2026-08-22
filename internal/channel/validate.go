package channel

import (
	"fmt"
	"net/url"
)

// Validate runs the checks every write path applies to a channel config.
//
// These are domain invariants, not HTTP concerns: what counts as a usable
// source type, key mode, or upstream URL is a property of a channel, and the
// admin API is only one caller. The error strings are nevertheless part of that
// API's response contract — clients key on the "mpd_url: " prefix — so they are
// reproduced verbatim rather than reworded.
func (c Config) Validate() error {
	if err := validateSourceURL(c.MPDURL); err != nil {
		return fmt.Errorf("mpd_url: %w", err)
	}
	if err := validateSourceType(c.SourceType); err != nil {
		return err
	}
	return validateKeyMode(c.HLSKeyMode)
}

// validateSourceType ensures the channel's upstream protocol is one we support.
// An empty value keeps the historical DASH default so configs written before
// HLS support remain valid.
func validateSourceType(s string) error {
	switch s {
	case "", SourceDASH, SourceHLS:
		return nil
	}
	return fmt.Errorf("source_type must be %q or %q", SourceDASH, SourceHLS)
}

// validateKeyMode ensures the AES-128 handling mode is one we support. Empty
// means passthrough, which is the safe default: it keeps upstream protection.
func validateKeyMode(s string) error {
	switch s {
	case "", KeyModePassthrough, KeyModeDecrypt:
		return nil
	}
	return fmt.Errorf("hls_key_mode must be %q or %q", KeyModePassthrough, KeyModeDecrypt)
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
