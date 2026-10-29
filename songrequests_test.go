package main

import (
	"context"
	"testing"

	"github.com/elianiva/meiro/internal/twitch"
	"github.com/elianiva/meiro/youtube"
)

func TestRequestQuery(t *testing.T) {
	for in, want := range map[string]string{
		"!sr daft punk around the world": "daft punk around the world",
		"!SR   spaced ":                  "spaced",
		"!songrequest a b":               "a b",
		"!sr":                            "",
		"hello !sr x":                    "",
		"!srx foo":                       "",
	} {
		if got := requestQuery(in); got != want {
			t.Errorf("requestQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSongRequestsQueueWithRequester(t *testing.T) {
	a := newTestApp(t)
	a.settings.AutoPlay = false
	a.requests.find = func(_ context.Context, query string) (youtube.MusicItem, error) {
		return youtube.MusicItem{VideoID: "id-" + query, Title: query, Kind: youtube.KindTrack}, nil
	}
	a.playFrom(youtube.MusicItem{VideoID: "now", Title: "Now"}, nil, 0, youtube.UpNextOptions{}, "Queue")

	a.handleChat(twitch.Message{User: "ana", Text: "!sr one"})
	a.handleChat(twitch.Message{User: "ana", Text: "!sr two"})
	a.handleChat(twitch.Message{User: "bob", Text: "just chatting"})
	a.handleChat(twitch.Message{User: "ana", Text: "!sr one"}) // already queued

	var got []string
	for i := range a.queue.len() {
		got = append(got, a.queue.at(i).VideoID+"|"+a.queue.at(i).RequestedBy)
	}
	want := []string{"now|", "id-one|ana", "id-two|ana"}
	if len(got) != len(want) {
		t.Fatalf("queue = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("queue = %v, want %v", got, want)
		}
	}
	if s := rowSubtitle(a.queue.at(1)); s != "Requested by ana" {
		t.Errorf("subtitle = %q", s)
	}

	a.handleChat(twitch.Message{User: "ana", Text: "!sr three"})
	a.handleChat(twitch.Message{User: "ana", Text: "!sr four"}) // over the limit
	if n := a.pendingRequests("ana"); n != maxPendingRequests {
		t.Errorf("pending = %d, want %d", n, maxPendingRequests)
	}
}

func TestSyncSongRequestsFollowsSettings(t *testing.T) {
	a := newTestApp(t)
	var channels []string
	a.requests.connect = func(ctx context.Context, channel string, _ func(twitch.Message), _ func(bool)) {
		channels = append(channels, channel)
	}
	a.settings.TwitchChannel = "Some_Channel"
	a.syncSongRequests()
	if len(channels) != 0 {
		t.Fatalf("connected while off: %v", channels)
	}
	a.settings.SongRequests = true
	a.syncSongRequests()
	a.syncSongRequests()
	if len(channels) != 1 || channels[0] != "some_channel" {
		t.Fatalf("channels = %v", channels)
	}
	a.settings.SongRequests = false
	a.syncSongRequests()
	if a.requests.cancel != nil {
		t.Error("still reading after being turned off")
	}
}

func TestVideoLinkID(t *testing.T) {
	for in, want := range map[string]string{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=5": "dQw4w9WgXcQ",
		"music.youtube.com/watch?v=dQw4w9WgXcQ":           "dQw4w9WgXcQ",
		"https://youtu.be/dQw4w9WgXcQ?si=x":               "dQw4w9WgXcQ",
		"https://youtube.com/shorts/dQw4w9WgXcQ":          "dQw4w9WgXcQ",
		"https://example.com/watch?v=dQw4w9WgXcQ":         "",
		"https://youtu.be/short":                          "",
		"daft punk":                                       "",
		"https://youtube.com/playlist?list=PLabc":         "",
	} {
		if got := videoLinkID(in); got != want {
			t.Errorf("videoLinkID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPickSongPrefersTheOfficialRecording(t *testing.T) {
	long := youtube.MusicItem{VideoID: "long", Title: "Get Lucky (10 Hour Loop)", Kind: youtube.KindVideo, Duration: "10:00:00"}
	live := youtube.MusicItem{VideoID: "live", Title: "Get Lucky (Live)", Kind: youtube.KindVideo, Duration: "5:00"}
	video := youtube.MusicItem{VideoID: "video", Title: "Daft Punk - Get Lucky (Official Audio)", Kind: youtube.KindVideo, Duration: "6:09"}
	song := youtube.MusicItem{VideoID: "song", Title: "Get Lucky", Kind: youtube.KindTrack, Duration: "6:09"}

	if got, _ := pickSong([]youtube.MusicItem{long, live, video, song}); got.VideoID != "song" {
		t.Errorf("picked %q, want the song", got.VideoID)
	}
	if got, _ := pickSong([]youtube.MusicItem{long, live, video}); got.VideoID != "video" {
		t.Errorf("picked %q, want the official video", got.VideoID)
	}
	if _, ok := pickSong([]youtube.MusicItem{long}); ok {
		t.Error("picked an hour-long loop")
	}
}
