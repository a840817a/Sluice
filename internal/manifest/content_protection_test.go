package manifest

import (
	"bytes"
	"encoding/xml"
	"strings"
	"testing"

	imdp "github.com/a840817a/sluice/internal/mpd"
)

// cpSourceMPD matches buildLocalFixture's layout (period "1", the first
// AdaptationSet, representation "1") so both manifests have published segments
// to list. contentProtections is spliced into the AdaptationSet.
func cpSourceMPD(rootNS, contentProtections string) string {
	return `<?xml version="1.0"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" ` + rootNS + ` type="static" mediaPresentationDuration="PT8S"
     profiles="urn:mpeg:dash:profile:isoff-main:2011">
  <Period id="1" duration="PT8S">
    <AdaptationSet mimeType="video/mp4">
      ` + contentProtections + `
      <SegmentTemplate timescale="90000" duration="180180" startNumber="1"
                       initialization="init.mp4" media="$Number%09d$.m4s"/>
      <Representation id="1" width="1920" height="1080" bandwidth="6000000" codecs="avc1.4d4028"/>
    </AdaptationSet>
  </Period>
</MPD>`
}

// unboundPrefixes returns every prefix doc uses without a declaration in
// scope. encoding/xml does not reject these even in strict mode, but a
// namespace-aware reader will.
func unboundPrefixes(t *testing.T, doc []byte) []string {
	t.Helper()
	var unbound []string
	var scopes []map[string]bool
	bound := func(p string) bool {
		for i := len(scopes) - 1; i >= 0; i-- {
			if scopes[i][p] {
				return true
			}
		}
		return false
	}
	dec := xml.NewDecoder(bytes.NewReader(doc))
	for {
		tok, err := dec.RawToken()
		if err != nil {
			break
		}
		switch tk := tok.(type) {
		case xml.StartElement:
			scope := map[string]bool{}
			for _, a := range tk.Attr {
				if a.Name.Space == "xmlns" {
					scope[a.Name.Local] = true
				}
			}
			scopes = append(scopes, scope)
			names := []xml.Name{tk.Name}
			for _, a := range tk.Attr {
				names = append(names, a.Name)
			}
			for _, n := range names {
				if n.Space != "" && n.Space != "xmlns" && n.Space != "xml" && !bound(n.Space) {
					unbound = append(unbound, n.Space+":"+n.Local)
				}
			}
		case xml.EndElement:
			scopes = scopes[:len(scopes)-1]
		}
	}
	return unbound
}

// generateBoth renders the served and the local manifest for src.
func generateBoth(t *testing.T, src string, cfg GeneratorConfig) (served, local []byte) {
	t.Helper()
	channelDir, ci, _ := buildLocalFixture(t)
	p, err := imdp.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	served, err = Generate(p, ci.Snapshot(), cfg)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	local, err = GenerateLocalMPD(p, ci.Snapshot(), channelDir)
	if err != nil {
		t.Fatalf("GenerateLocalMPD: %v", err)
	}
	return served, local
}

var cpTestConfig = GeneratorConfig{GatewayBaseURL: "https://gw.example.com", ChannelID: "ch"}

// Both manifests carry the source's ContentProtection elements with their
// literal prefixes — DRM tools match cenc: and mspr: as text — and every
// prefix they use is declared.
func TestManifestsKeepTheSourceSpellingOfContentProtection(t *testing.T) {
	const cps = `<ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" value="cenc" cenc:default_KID="11111111-2222-3333-4444-555555555555"/>
      <ContentProtection schemeIdUri="urn:uuid:9a04f079-9840-4286-ab92-e65be0885f95" value="MSPR 2.0">
        <cenc:pssh>AAAA</cenc:pssh>
        <mspr:pro>BBBB</mspr:pro>
      </ContentProtection>`
	src := cpSourceMPD(`xmlns:cenc="urn:mpeg:cenc:2013" xmlns:mspr="urn:microsoft:playready"`, cps)
	served, local := generateBoth(t, src, cpTestConfig)

	for name, doc := range map[string][]byte{"served": served, "local": local} {
		for _, want := range []string{
			`cenc:default_KID="11111111-2222-3333-4444-555555555555"`,
			`<cenc:pssh>AAAA</cenc:pssh>`,
			`<mspr:pro>BBBB</mspr:pro>`,
		} {
			if !strings.Contains(string(doc), want) {
				t.Errorf("%s manifest lacks %s:\n%s", name, want, doc)
			}
		}
		if u := unboundPrefixes(t, doc); len(u) > 0 {
			t.Errorf("%s manifest uses undeclared prefixes %v:\n%s", name, u, doc)
		}
	}
}

// A source is free to quote attributes with ' and to name the cenc namespace
// with any prefix. Neither may cost the element its place in the served
// manifest or make it invalid XML.
func TestServedManifestKeepsContentProtectionWrittenUnconventionally(t *testing.T) {
	const cp = `<ContentProtection schemeIdUri='urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed' ns2:default_KID='11111111-2222-3333-4444-555555555555'>
        <ns2:pssh>CCCC</ns2:pssh>
      </ContentProtection>`
	src := cpSourceMPD(`xmlns:ns2="urn:mpeg:cenc:2013"`, cp)
	served, _ := generateBoth(t, src, cpTestConfig)

	if n := strings.Count(string(served), "<ContentProtection"); n != 1 {
		t.Fatalf("served manifest has %d ContentProtection elements, want 1:\n%s", n, served)
	}
	for _, want := range []string{
		`ns2:default_KID='11111111-2222-3333-4444-555555555555'`,
		`<ns2:pssh>CCCC</ns2:pssh>`,
	} {
		if !strings.Contains(string(served), want) {
			t.Errorf("served manifest lacks %s — the source element was replaced or dropped:\n%s", want, served)
		}
	}
	if u := unboundPrefixes(t, served); len(u) > 0 {
		t.Errorf("served manifest uses undeclared prefixes %v:\n%s", u, served)
	}
}
