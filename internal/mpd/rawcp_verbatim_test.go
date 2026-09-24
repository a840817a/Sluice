package mpd

import "testing"

// DRM tools match the literal cenc: and mspr: prefixes, so a ContentProtection
// element must come out of the parser spelled as the source spelled it — not
// merely as namespace-equivalent XML.
func TestRawCPsKeepTheSourceSpelling(t *testing.T) {
	const cencCP = `<ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" value="cenc" cenc:default_KID="11111111-2222-3333-4444-555555555555"/>`
	const prCP = `<ContentProtection schemeIdUri="urn:uuid:9a04f079-9840-4286-ab92-e65be0885f95" value="MSPR 2.0">
        <cenc:pssh>AAAA</cenc:pssh>
        <mspr:pro>BBBB</mspr:pro>
      </ContentProtection>`
	const singleQuoted = `<ContentProtection schemeIdUri='urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed' cenc:default_KID='11111111-2222-3333-4444-555555555555'/>`

	tests := []struct {
		name    string
		rootNS  string
		source  string
		wantRaw string
	}{
		{
			name:    "prefixed attribute",
			rootNS:  `xmlns:cenc="urn:mpeg:cenc:2013"`,
			source:  cencCP,
			wantRaw: cencCP,
		},
		{
			name:    "prefixed children",
			rootNS:  `xmlns:cenc="urn:mpeg:cenc:2013" xmlns:mspr="urn:microsoft:playready"`,
			source:  prCP,
			wantRaw: prCP,
		},
		{
			name:    "single-quoted attributes",
			rootNS:  `xmlns:cenc="urn:mpeg:cenc:2013"`,
			source:  singleQuoted,
			wantRaw: singleQuoted,
		},
		{
			// The output documents declare only cenc and mspr, so any other
			// prefix the element borrows from an ancestor travels with it.
			name:    "non-standard prefix declared on the root",
			rootNS:  `xmlns:ns2="urn:mpeg:cenc:2013"`,
			source:  `<ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" ns2:default_KID="k"/>`,
			wantRaw: `<ContentProtection xmlns:ns2="urn:mpeg:cenc:2013" schemeIdUri="urn:mpeg:dash:mp4protection:2011" ns2:default_KID="k"/>`,
		},
		{
			// The source binds cenc to something else: the output root's cenc
			// declaration would be wrong for it, so it carries its own.
			name:    "standard prefix bound to another namespace",
			rootNS:  `xmlns:cenc="urn:example:other"`,
			source:  `<ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" cenc:default_KID="k"/>`,
			wantRaw: `<ContentProtection xmlns:cenc="urn:example:other" schemeIdUri="urn:mpeg:dash:mp4protection:2011" cenc:default_KID="k"/>`,
		},
		{
			name:    "prefix declared on the AdaptationSet",
			rootNS:  `xmlns:cenc="urn:mpeg:cenc:2013"`,
			source:  `<ContentProtection schemeIdUri="urn:uuid:x"><dashif:laurl>https://l.example</dashif:laurl></ContentProtection>`,
			wantRaw: `<ContentProtection xmlns:dashif="https://dashif.org/CPS" schemeIdUri="urn:uuid:x"><dashif:laurl>https://l.example</dashif:laurl></ContentProtection>`,
		},
		{
			// The element's own declaration wins over the root's binding.
			name:    "prefix declared inside the element",
			rootNS:  `xmlns:cenc="urn:example:other"`,
			source:  `<ContentProtection schemeIdUri="urn:uuid:x" xmlns:cenc="urn:mpeg:cenc:2013"><cenc:pssh>AAAA</cenc:pssh></ContentProtection>`,
			wantRaw: `<ContentProtection schemeIdUri="urn:uuid:x" xmlns:cenc="urn:mpeg:cenc:2013"><cenc:pssh>AAAA</cenc:pssh></ContentProtection>`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := `<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" ` + tc.rootNS + ` type="static" mediaPresentationDuration="PT4S">
  <Period id="p0" duration="PT4S">
    <AdaptationSet mimeType="video/mp4" xmlns:dashif="https://dashif.org/CPS">
      ` + tc.source + `
      <SegmentTemplate timescale="1" media="v_$Number$.m4s" initialization="init.mp4" duration="2" startNumber="1"/>
      <Representation id="v1" bandwidth="1000"/>
    </AdaptationSet>
  </Period>
</MPD>`
			p, err := Parse([]byte(doc))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			raws := p.Periods[0].AdaptationSets[0].RawCPs
			if len(raws) != 1 {
				t.Fatalf("got %d RawCPs, want 1: %q", len(raws), raws)
			}
			if raws[0] != tc.wantRaw {
				t.Errorf("RawCPs[0]:\n got  %s\n want %s", raws[0], tc.wantRaw)
			}
		})
	}
}
