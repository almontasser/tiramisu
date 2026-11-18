package torr

import "testing"

// A fully cached episode under confirmed playback has no reader between reads and no
// download speed; it must still be listed as streaming.
func TestIsStreamingCountsConfirmedPlayback(t *testing.T) {
	tr := &Torrent{}
	if tr.IsStreaming() {
		t.Fatal("an idle torrent is not streaming")
	}
	tr.IsPriority.Store(true)
	if !tr.IsStreaming() {
		t.Fatal("a torrent with confirmed playback is streaming")
	}
}
