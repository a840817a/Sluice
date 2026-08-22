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

func TestGenerateStaticMPD(t *testing.T) {
	data, _ := os.ReadFile("../../testdata/manifest.mpd")
	p, err := imdp.Parse(data)
	if err != nil {
		t.Fatal(err)
	}

	ci := index.NewChannelIndex()
	cfg := GeneratorConfig{
		GatewayBaseURL: "https://gw.example.com",
		ChannelID:      "ch1",
		LicenseURL:     "https://gw.example.com/v1/channels/ch1/license",
	}

	out, err := Generate(p, ci.Snapshot(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	xml := string(out)
	t.Logf("Generated MPD:\n%s", xml)

	// Segment URLs must point to gateway, not origin
	if strings.Contains(xml, "manifest_6m_") {
		t.Error("output contains original upstream segment filename")
	}
	if !strings.Contains(xml, "v1/channels/ch1/segments/") {
		t.Error("output does not contain gateway segment path")
	}
	if !strings.Contains(xml, "v1/channels/ch1/init/") {
		t.Error("output does not contain gateway init path")
	}

	// ContentProtection elements must be preserved
	if !strings.Contains(xml, "ContentProtection") {
		t.Error("ContentProtection elements missing from output")
	}

	// Gateway license URL must appear inside the mspr:pro blob.
	// The <pro> element contains base64(UTF-16LE XML) — decode and verify.
	re := regexp.MustCompile(`<pro[^>]*>([A-Za-z0-9+/=\s]+)</pro>`)
	proMatches := re.FindAllStringSubmatch(xml, -1)
	if len(proMatches) == 0 {
		t.Fatal("no <pro> elements found in output")
	}
	found := false
	for _, m := range proMatches {
		b64 := strings.TrimSpace(m[1])
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			continue
		}
		decoded, err := utf16LEToString(raw)
		if err != nil {
			continue
		}
		if strings.Contains(decoded, "v1/channels/ch1/license") {
			found = true
			break
		}
	}
	if !found {
		t.Error("gateway license URL not found inside any mspr:pro blob")
	}
}

func TestGenerateStaticMPD_DisablesSelectedDRM(t *testing.T) {
	data, _ := os.ReadFile("../../testdata/manifest.mpd")
	p, err := imdp.Parse(data)
	if err != nil {
		t.Fatal(err)
	}

	ci := index.NewChannelIndex()
	cfg := GeneratorConfig{
		GatewayBaseURL:  "https://gw.example.com",
		ChannelID:       "ch1",
		LicenseURL:      "https://gw.example.com/v1/channels/ch1/license/playready",
		DisableWidevine: true,
	}

	out, err := Generate(p, ci.Snapshot(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	xml := string(out)
	if strings.Contains(xml, "edef8ba9-79d6-4ace-a3c8-27dcd51d21ed") {
		t.Fatal("expected Widevine ContentProtection to be omitted")
	}
	if !strings.Contains(xml, "9a04f079-9840-4286-ab92-e65be0885f95") {
		t.Fatal("expected PlayReady ContentProtection to remain")
	}
}
