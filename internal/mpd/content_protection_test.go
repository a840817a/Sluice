package mpd

import (
	"strings"
	"testing"
)

func TestParseContentProtectionsStructured(t *testing.T) {
	data := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT4S">
  <Period id="p0" duration="PT4S">
    <AdaptationSet contentType="video" mimeType="video/mp4">
      <ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" value="cenc" xmlns:cenc="urn:mpeg:cenc:2013" cenc:default_KID="0ec16ee1-85e5-3f17-8347-eeb388be70f8"/>
      <ContentProtection schemeIdUri="urn:uuid:9a04f079-9840-4286-ab92-e65be0885f95" value="MSPR 2.0">
        <cenc:pssh xmlns:cenc="urn:mpeg:cenc:2013">playready-pssh</cenc:pssh>
      </ContentProtection>
      <ContentProtection schemeIdUri="urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed">
        <cenc:pssh xmlns:cenc="urn:mpeg:cenc:2013">widevine-pssh</cenc:pssh>
      </ContentProtection>
      <SegmentTemplate timescale="1" media="v_$Number$.m4s" initialization="init.mp4" duration="2" startNumber="1"/>
      <Representation id="v1" bandwidth="1000" codecs="avc1.4D401F"/>
    </AdaptationSet>
  </Period>
</MPD>`)

	p, err := Parse(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	as := p.Periods[0].AdaptationSets[0]
	if len(as.ParsedContentProtections) != 3 {
		t.Fatalf("expected 3 parsed content protections, got %d", len(as.ParsedContentProtections))
	}

	cenc := as.ParsedContentProtections[0]
	if cenc.System != ContentProtectionCENC {
		t.Fatalf("expected first CP to be CENC, got %s", cenc.System)
	}
	if cenc.DefaultKID != "0ec16ee1-85e5-3f17-8347-eeb388be70f8" {
		t.Fatalf("default KID not parsed: %q", cenc.DefaultKID)
	}

	playready := as.ParsedContentProtections[1]
	if playready.System != ContentProtectionPlayReady {
		t.Fatalf("expected PlayReady, got %s", playready.System)
	}
	if playready.Value != "MSPR 2.0" {
		t.Fatalf("PlayReady value not parsed: %q", playready.Value)
	}
	if len(playready.PSSHs) != 1 || playready.PSSHs[0] != "playready-pssh" {
		t.Fatalf("PlayReady PSSH not parsed: %#v", playready.PSSHs)
	}
	if !strings.Contains(playready.RawXML, "playready-pssh") {
		t.Fatal("PlayReady raw XML was not retained")
	}

	widevine := as.ParsedContentProtections[2]
	if widevine.System != ContentProtectionWidevine {
		t.Fatalf("expected Widevine, got %s", widevine.System)
	}
	if len(widevine.PSSHs) != 1 || widevine.PSSHs[0] != "widevine-pssh" {
		t.Fatalf("Widevine PSSH not parsed: %#v", widevine.PSSHs)
	}
}
