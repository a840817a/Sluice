package web

import (
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPlayerLoadsEmbeddedPlaybackModule(t *testing.T) {
	page := httptest.NewRecorder()
	PlayerHandler().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/player/example", nil))
	if page.Code != http.StatusOK {
		t.Fatalf("player status = %d", page.Code)
	}
	const assetPath = "/player-assets/player/playback.mjs"
	if !strings.Contains(page.Body.String(), `from "`+assetPath+`"`) {
		t.Fatal("player does not import the playback module")
	}
	asset := httptest.NewRecorder()
	PlayerAssetsHandler().ServeHTTP(asset, httptest.NewRequest(http.MethodGet, assetPath, nil))
	if asset.Code != http.StatusOK {
		t.Fatalf("playback module status = %d", asset.Code)
	}
	mediaType, _, err := mime.ParseMediaType(asset.Header().Get("Content-Type"))
	if err != nil || (mediaType != "text/javascript" && mediaType != "application/javascript") {
		t.Fatalf("playback module cannot be imported: Content-Type = %q", asset.Header().Get("Content-Type"))
	}
	if !strings.Contains(asset.Body.String(), "export function createPlayback") {
		t.Fatal("embedded asset does not contain the playback module")
	}
}
