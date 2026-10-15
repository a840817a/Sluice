package channel

// Converting the AdaptationSets a DASH decrypt channel stored as ciphertext
// once it holds their keys: keys added later, or found at start.

import (
	"context"
	"errors"
	"log/slog"

	"github.com/a840817a/sluice/internal/pipeline"
)

// launchConversions starts rt's conversion goroutine, if rt decrypts. Like
// every goroutine of the runtime it is counted in rt.wg, so m.mu must be held
// with rt published.
func (m *Manager) launchConversions(ctx context.Context, rt *Runtime) {
	if rt.decrypt == nil {
		return
	}
	rt.wg.Add(1)
	go func() { defer rt.wg.Done(); m.runConversions(ctx, rt) }()
}

// runConversions converts, one at a time, every AdaptationSet the registry
// finds convertible: once at start, after the index and the registry were
// restored, and again each time the registry wakes it.
func (m *Manager) runConversions(ctx context.Context, rt *Runtime) {
	for {
		pending, err := rt.decrypt.Convertible()
		if err != nil {
			slog.Warn("listing AdaptationSets to convert failed", "channel", rt.ID, "err", err)
		}
		for _, c := range pending {
			if ctx.Err() != nil {
				return
			}
			if rt.servesSavedManifestOnly() {
				rt.decrypt.Refuse(c, "this VOD serves the manifest it saved, without the origin MPD "+
					"to render a new one from, so the manifest could not stop protecting it")
				continue
			}
			if err := rt.decrypt.Convert(ctx, c, rt.exclusiveMedia); err == nil {
				m.afterConversion(rt)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-rt.decrypt.Wake():
		}
	}
}

// servesSavedManifestOnly reports whether rt is a VOD restored without its
// origin MPD, which serves the manifest its transition saved as it is. Its
// media is never converted: nothing could re-describe it.
func (rt *Runtime) servesSavedManifestOnly() bool {
	return rt.ParsedMPD() == nil && rt.savedDASHManifest() != nil
}

// exclusiveMedia runs fn while nothing else writes rt's stored media: on the
// processor's goroutine while it ingests, and otherwise under mediaMu, which
// keeps it apart from the VOD transition. The processor stops for good when
// the VOD transition joins it, and a runtime started in VOD never runs one.
func (rt *Runtime) exclusiveMedia(ctx context.Context, fn func() error) error {
	if rt.processor != nil {
		err := rt.processor.Exclusive(ctx, fn)
		if !errors.Is(err, pipeline.ErrStopped) || ctx.Err() != nil {
			return err
		}
	}
	rt.mediaMu.Lock()
	defer rt.mediaMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

// afterConversion rewrites what a VOD channel saved from its manifest, now
// that an AdaptationSet it describes is clear. What is served follows on its
// own: ParsedMPD strips the ContentProtection by the registry's generation.
func (m *Manager) afterConversion(rt *Runtime) {
	rt.mediaMu.Lock()
	defer rt.mediaMu.Unlock()
	if rt.Mode() != ModeVOD || rt.ParsedMPD() == nil {
		// A VOD transition still to come saves the manifest; a VOD without
		// an MPD converts nothing (servesSavedManifestOnly).
		return
	}
	rt.configMu.Lock()
	a, err := m.vod.capture(rt, rt.VODIndex())
	rt.configMu.Unlock()
	if err != nil {
		slog.Warn("VOD manifest not rewritten after conversion", "channel", rt.ID, "err", err)
		return
	}
	for _, w := range a.persistDASHManifests() {
		slog.Warn("VOD manifest rewrite after conversion: "+w, "channel", rt.ID)
	}
	a.writeOptional()
}

// urlVersion is what presentation.Channel.URLVersion is for rt: nil unless
// rt decrypts.
func (rt *Runtime) urlVersion() func(periodID, asID, repID string) uint64 {
	if rt.decrypt == nil {
		return nil
	}
	return rt.decrypt.URLVersion
}
