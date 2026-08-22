package mpd

import (
	"fmt"
	"os"
	"testing"
)

func TestRawCPs(t *testing.T) {
	data, _ := os.ReadFile("../../testdata/manifest.mpd")
	rawCPs := extractRawContentProtections(data)
	for i, asCPs := range rawCPs {
		fmt.Printf("AS[%d]: %d CPs\n", i, len(asCPs))
		for j, cp := range asCPs {
			fmt.Printf("  CP[%d]: %s\n", j, cp[:min2(len(cp), 200)])
		}
	}
}
func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}
