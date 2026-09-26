package manifest

import (
	"encoding/base64"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
)

// The LA_URL inside a PlayReady mspr:pro blob is what a player contacts for a
// licence. Rewriting it to point at this gateway is the entire reason the licence
// proxy exists: miss one and that player talks to the upstream licence server
// directly, bypassing the proxy's headers, its credentials and its logging —
// and it works, which is why nothing complains.

// proBlob renders XML the way an mspr:pro attribute carries it: UTF-16LE, base64.
func proBlob(t *testing.T, xml string) string {
	t.Helper()
	u16, err := stringToUTF16LE(xml)
	if err != nil {
		t.Fatalf("encode UTF-16LE: %v", err)
	}
	return base64.StdEncoding.EncodeToString(u16)
}

// laURLsIn decodes every pro blob in an MPD — in either rendering — and returns
// the LA_URL each one carries.
func laURLsIn(t *testing.T, doc string) []string {
	t.Helper()
	var out []string
	// <mspr:pro> is the usual upstream spelling; <pro ...> covers an upstream
	// that binds the namespace as the default on the element itself.
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`(?s)<mspr:pro>(.*?)</mspr:pro>`),
		regexp.MustCompile(`(?s)<pro[^>]*>(.*?)</pro>`),
	} {
		for _, m := range re.FindAllStringSubmatch(doc, -1) {
			raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(m[1]))
			if err != nil {
				continue
			}
			s, err := utf16LEToString(raw)
			if err != nil {
				continue
			}
			if i := strings.Index(s, "<LA_URL>"); i >= 0 {
				if j := strings.Index(s[i:], "</LA_URL>"); j >= 0 {
					out = append(out, s[i+len("<LA_URL>"):i+j])
				}
			}
		}
	}
	return out
}

const wrmHeader = `<WRMHEADER xmlns="http://schemas.microsoft.com/DRM/2007/03/PlayReadyHeader" version="4.0.0.0">` +
	`<DATA><PROTECTINFO><KEYLEN>16</KEYLEN><ALGID>AESCTR</ALGID></PROTECTINFO>` +
	`<KID>q4fbGkJ1EeqlAAAAAAAAAA==</KID>` +
	`<LA_URL>https://upstream.example/license</LA_URL>` +
	`<CHECKSUM>abcdefgh</CHECKSUM></DATA></WRMHEADER>`

// The end that matters: no player is ever handed the upstream licence server's
// address. This asserts the result in the output rather than the rewrite that
// produces it.
func TestGeneratedManifestNeverPointsAtTheUpstreamLicenceServer(t *testing.T) {
	data, err := os.ReadFile("../../testdata/manifest.mpd")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	p, err := imdp.Parse(data)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}

	const upstream = "https://dummy.drmasp-nextscape.net/license"
	if urls := laURLsIn(t, string(data)); len(urls) == 0 {
		t.Fatal("the fixture carries no LA_URL, so this test proves nothing")
	} else {
		for _, u := range urls {
			if u != upstream {
				t.Fatalf("fixture LA_URL = %q, want %q — update this test", u, upstream)
			}
		}
	}

	const gateway = "https://gw.example.com/v1/channels/ch1/license"
	out, err := Generate(p, index.NewChannelIndex(index.PublishAVBarrier).Snapshot(), GeneratorConfig{
		GatewayBaseURL: "https://gw.example.com",
		ChannelID:      "ch1",
		LicenseURL:     gateway,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	urls := laURLsIn(t, string(out))
	if len(urls) == 0 {
		t.Fatal("the generated manifest carries no LA_URL at all; protection signalling was lost")
	}
	for _, u := range urls {
		if u == upstream {
			t.Error("a player following this manifest would contact the upstream licence " +
				"server directly, bypassing the gateway's proxy")
		}
		if u != gateway {
			t.Errorf("LA_URL = %q, want the gateway's proxy %q", u, gateway)
		}
	}
}

// The same assertion for a minimal upstream that writes the namespace-prefixed
// spelling. The parser keeps ContentProtection elements as source bytes, so
// <mspr:pro> reaches the rewrite spelled exactly so, and the rewrite has to
// find the element by namespace rather than by its literal name.
func TestGeneratedManifestRewritesTheNamespacePrefixedSpelling(t *testing.T) {
	raw := `<?xml version="1.0" encoding="utf-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" xmlns:mspr="urn:microsoft:playready"
     type="static" mediaPresentationDuration="PT12S" minBufferTime="PT2S"
     profiles="urn:mpeg:dash:profile:isoff-on-demand:2011">
  <Period id="p0" duration="PT12S">
    <AdaptationSet id="1" mimeType="video/mp4" contentType="video">
      <ContentProtection schemeIdUri="urn:uuid:9a04f079-9840-4286-ab92-e65be0885f95" value="MSPR 2.0">
        <mspr:pro>` + proBlob(t, wrmHeader) + `</mspr:pro>
      </ContentProtection>
      <SegmentTemplate timescale="1000" duration="2000" startNumber="1"
                       initialization="init.mp4" media="v0_$Number%09d$.m4s"/>
      <Representation id="v0" bandwidth="800000" codecs="avc1.64001f" width="640" height="360"/>
    </AdaptationSet>
  </Period>
</MPD>`

	p, err := imdp.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	const gateway = "https://gw.example.com/v1/channels/ch1/license"
	out, err := Generate(p, index.NewChannelIndex(index.PublishAVBarrier).Snapshot(), GeneratorConfig{
		GatewayBaseURL: "https://gw.example.com",
		ChannelID:      "ch1",
		LicenseURL:     gateway,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	urls := laURLsIn(t, string(out))
	if len(urls) == 0 {
		t.Fatalf("no LA_URL survived into the output at all:\n%s", out)
	}
	for _, u := range urls {
		if u != gateway {
			t.Errorf("LA_URL = %q, want the gateway's proxy %q — an <mspr:pro> upstream "+
				"is still pointing players at its own licence server", u, gateway)
		}
	}
}
