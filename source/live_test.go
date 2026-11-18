package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func init() {
	// Relay tests move megabytes; pacing would only make them slow.
	relayPace = 1 << 40
}

func TestLiveAudioPlaylistPicksAACRendition(t *testing.T) {
	master := `#EXTM3U
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-MEDIA:URI="https://manifest.googlevideo.com/api/manifest/hls_playlist/itag/233/index.m3u8",TYPE=AUDIO,GROUP-ID="233",NAME="Default",DEFAULT=YES,AUTOSELECT=YES
#EXT-X-MEDIA:URI="https://manifest.googlevideo.com/api/manifest/hls_playlist/itag/234/index.m3u8",TYPE=AUDIO,GROUP-ID="234",NAME="Default",DEFAULT=YES,AUTOSELECT=YES
#EXT-X-STREAM-INF:BANDWIDTH=5420722,CODECS="avc1.640028,mp4a.40.2",RESOLUTION=1920x1080,AUDIO="234"
https://manifest.googlevideo.com/api/manifest/hls_playlist/itag/270/index.m3u8
`
	c := newCatalog()
	c.http = &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(master))}, nil
	})}
	got, err := c.liveAudioPlaylist(context.Background(), "https://manifest.googlevideo.com/api/manifest/hls_variant/index.m3u8")
	if err != nil || !strings.Contains(got, "/itag/234/") {
		t.Fatalf("got %q, %v", got, err)
	}
}
