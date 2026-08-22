package mpd

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestRawCPsFull(t *testing.T) {
	data, _ := os.ReadFile("../../testdata/manifest.mpd")
	rawCPs := extractRawContentProtections(data)
	for _, cp := range rawCPs[0] {
		fmt.Printf("len=%d hasPro=%v hasPssh=%v\n", len(cp),
			strings.Contains(cp, "pro"), strings.Contains(cp, "pssh"))
	}
}
