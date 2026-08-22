package hlsout

import (
	"strings"
	"testing"

	"github.com/a840817a/sluice/internal/hlsdesc"
	"github.com/a840817a/sluice/internal/index"
)

// An empty GatewayBaseURL is the documented default: the gateway is reachable
// at several addresses at once (localhost, a LAN IP, behind a proxy) and a
// playlist that hardcodes one of them breaks the others. HLS resolves relative
// URIs against the playlist's own URL, so root-relative output serves them all.

func TestMasterEmptyBaseIsRootRelative(t *testing.T) {
	variants := []hlsdesc.Variant{{RepID: "v0", Bandwidth: 800000}}
	out := string(Master(variants, MasterOptions{ChannelID: "ch1"}))

	if strings.Contains(out, "://") {
		t.Errorf("empty base still produced an absolute URI:\n%s", out)
	}
	if !strings.Contains(out, "\n/v1/channels/ch1/media/v0.m3u8\n") {
		t.Errorf("no root-relative media playlist URI:\n%s", out)
	}
}

func TestMediaEmptyBaseIsRootRelative(t *testing.T) {
	o := mediaOpts()
	o.GatewayBaseURL = ""
	out := string(Media([]index.SegmentState{seg(1, 0, 6), seg(2, 6, 6)}, o))

	if strings.Contains(out, "://") {
		t.Errorf("empty base still produced an absolute URI:\n%s", out)
	}
	if !strings.Contains(out, "\n/v1/channels/ch1/segments/v0/1.ts\n") {
		t.Errorf("no root-relative segment URI:\n%s", out)
	}
}

// The configured base must still win when it is set: relative output is the
// default, not the only mode. A sub-path reverse proxy needs the override.
func TestNonEmptyBaseStaysAbsolute(t *testing.T) {
	out := string(Media([]index.SegmentState{seg(1, 0, 6)}, mediaOpts()))
	if !strings.Contains(out, "https://gw.example.com/v1/channels/ch1/segments/v0/1.ts") {
		t.Errorf("configured base was not honoured:\n%s", out)
	}
}
