package manifest

import (
	"os"
	"strings"
	"testing"

	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
)

// An empty GatewayBaseURL is the documented default. DASH resolves the MPD's
// <BaseURL> against the URL the manifest was fetched from (RFC 3986), so "/"
// makes one manifest correct at localhost, at a LAN IP over TLS, and behind a
// reverse proxy, without the gateway knowing any of those addresses.
func TestGenerateEmptyBaseIsRootRelative(t *testing.T) {
	data, err := os.ReadFile("../../testdata/manifest.mpd")
	if err != nil {
		t.Fatal(err)
	}
	p, err := imdp.Parse(data)
	if err != nil {
		t.Fatal(err)
	}

	ci := index.NewChannelIndex()
	out, err := Generate(p, ci.Snapshot(), GeneratorConfig{ChannelID: "ch1"})
	if err != nil {
		t.Fatal(err)
	}
	xml := string(out)

	if !strings.Contains(xml, "<BaseURL>/</BaseURL>") {
		t.Errorf("expected a root BaseURL element, got:\n%s", xml)
	}
	// Segment and init templates are relative to <BaseURL> and must stay that
	// way; an absolute gateway URL anywhere means the base leaked into them.
	for _, bad := range []string{"http://", "https://gw."} {
		if strings.Contains(xml, bad) {
			t.Errorf("empty base still produced an absolute gateway URI (%q):\n%s", bad, xml)
		}
	}
	if !strings.Contains(xml, "v1/channels/ch1/segments/") {
		t.Errorf("segment template missing:\n%s", xml)
	}
}
