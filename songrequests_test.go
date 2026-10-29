package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	a.requests.token = "validated"
	var replies []string
	a.requests.sendChat = func(message string) error {
		replies = append(replies, message)
		return nil
	}
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
	if len(replies) != 2 || !strings.Contains(replies[0], "posición 2") || !strings.Contains(replies[1], "posición 3") {
		t.Errorf("replies = %v, want positions 2 and 3", replies)
	}

	a.handleChat(twitch.Message{User: "ana", Text: "!sr three"})
	a.handleChat(twitch.Message{User: "ana", Text: "!sr four"}) // over the limit
	if n := a.pendingRequests("ana"); n != maxPendingRequests {
		t.Errorf("pending = %d, want %d", n, maxPendingRequests)
	}
	if len(replies) != 3 || !strings.Contains(replies[2], "posición 4") {
		t.Errorf("replies = %v, want third song at position 4", replies)
	}
}

func TestRequestedPositionCountsFromCurrent(t *testing.T) {
	q := newQueue()
	q.items = []youtube.MusicItem{
		{VideoID: "previous"},
		{VideoID: "current"},
		{VideoID: "request", RequestedBy: "ana"},
	}
	q.index = 1
	if got := requestedPosition(&q, "request", "ana"); got != 2 {
		t.Errorf("position = %d, want 2", got)
	}
	if got := requestedPosition(&q, "previous", "ana"); got != 0 {
		t.Errorf("played request position = %d, want 0", got)
	}
}

func TestRadioAnnouncementUsesRequestAndTrackDetails(t *testing.T) {
	previous := youtube.MusicItem{VideoID: "one", Title: "One More Time", Artist: "Daft Punk", RequestedBy: "ana", Kind: youtube.KindTrack}
	next := youtube.MusicItem{VideoID: "two", Title: "Get Lucky", Artist: "Daft Punk", RequestedBy: "bob", Kind: youtube.KindTrack}
	line := radioAnnouncement(previous, next)
	for _, part := range []string{"One More Time", "Daft Punk", "ana", "Radio Chatcito", "Get Lucky", "bob"} {
		if !strings.Contains(line, part) {
			t.Errorf("announcement %q does not contain %q", line, part)
		}
	}
	if !requestedSong(previous) || !requestedSong(next) {
		t.Fatal("song requests were not recognized")
	}
	if requestedSong(youtube.MusicItem{VideoID: "other", Kind: youtube.KindTrack}) {
		t.Fatal("a song not requested in chat was recognized as a request")
	}
	if line := radioAnnouncement(previous, youtube.MusicItem{RequestedBy: "bob"}); line != "" {
		t.Errorf("announcement without a title = %q", line)
	}
}

func TestRadioPlanningIsStableAndRequestOnly(t *testing.T) {
	a := newTestApp(t)
	a.settings.SongRequests = true
	a.requests.ttsKey = "key"
	a.current = youtube.MusicItem{VideoID: "previous", RequestedBy: "ana", Kind: youtube.KindTrack}
	a.queue.items = []youtube.MusicItem{
		a.current,
		{VideoID: "next", RequestedBy: "bob", Kind: youtube.KindTrack},
	}
	a.queue.index = 0
	rolls := 0
	a.requests.radioRoll = func() bool {
		rolls++
		return true
	}
	next := a.queue.at(1)
	if !a.radioPlanned(1, next) || !a.radioPlanned(1, next) || rolls != 1 {
		t.Fatalf("radio plan changed for the same transition: chosen=%v rolls=%d", a.requests.radioChosen, rolls)
	}
	a.settings.SongRequests = false
	if a.radioPlanned(1, next) {
		t.Fatal("announcer planned while Song requests is off")
	}
}

func TestSyncSongRequestsFollowsSettings(t *testing.T) {
	a := newTestApp(t)
	var channels []string
	var authenticated []string
	a.requests.connect = func(ctx context.Context, channel string, _ func(twitch.Message), _ func(bool)) {
		channels = append(channels, channel)
	}
	a.requests.connectAuthenticated = func(_ context.Context, channel, login, token string, _ func(twitch.Message), _ func(bool), _ func(func(string) error)) {
		authenticated = append(authenticated, channel+"|"+login+"|"+token)
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
	a.requests.token, a.requests.login = "access-token", "account"
	a.syncSongRequests()
	if len(authenticated) != 1 || authenticated[0] != "some_channel|account|access-token" {
		t.Fatalf("authenticated connections = %v", authenticated)
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

func TestDiscardRadioClipRemovesTheFile(t *testing.T) {
	a := newTestApp(t)
	path := filepath.Join(t.TempDir(), "clip.mp3")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.requests.clipLine, a.requests.clipPath = "line", path
	a.discardRadioClip()
	if _, err := os.Stat(path); !os.IsNotExist(err) || a.requests.clipLine != "" || a.requests.clipPath != "" {
		t.Fatalf("clip kept: %v %q %q", err, a.requests.clipLine, a.requests.clipPath)
	}
}

func TestRadioAlwaysForcesEveryTransition(t *testing.T) {
	a := newTestApp(t)
	a.settings.SongRequests = true
	a.requests.ttsKey = "key"
	a.current = youtube.MusicItem{VideoID: "a", Title: "A", Kind: youtube.KindTrack}
	next := youtube.MusicItem{VideoID: "b", Title: "B", Kind: youtube.KindTrack}
	if a.radioPlanned(1, next) {
		t.Fatal("planned for songs nobody requested")
	}
	a.settings.RadioAlways = true
	if !a.radioPlanned(1, next) {
		t.Fatal("not forced")
	}
	a.settings.SongRequests = false
	if a.radioPlanned(1, next) {
		t.Fatal("forced while Song requests is off")
	}
}

func TestRadioStaysSilentWithoutKey(t *testing.T) {
	a := newTestApp(t)
	a.settings.SongRequests, a.settings.RadioAlways = true, true
	if a.radioPlanned(1, youtube.MusicItem{VideoID: "b"}) {
		t.Fatal("planned a break with no Text-to-Speech key")
	}
}

func TestGoogleSpeech(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") != "k" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"API key not valid"}}`))
			return
		}
		var body struct {
			Input struct{ Text string }
			Voice struct{ Name string }
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Input.Text != "hola" || body.Voice.Name == "" {
			t.Errorf("request = %+v", body)
		}
		_, _ = w.Write([]byte(`{"audioContent":"` + base64.StdEncoding.EncodeToString([]byte("mp3")) + `"}`))
	}))
	defer server.Close()
	audio, err := googleSpeech(context.Background(), server.URL, "k", "hola")
	if err != nil || string(audio) != "mp3" {
		t.Fatalf("audio %q, err %v", audio, err)
	}
	if _, err := googleSpeech(context.Background(), server.URL, "bad", "hola"); err == nil || !strings.Contains(err.Error(), "API key not valid") {
		t.Fatalf("err = %v", err)
	}
}

func TestRadioAnnouncementVariesButIsStablePerPair(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 40; i++ {
		previous := youtube.MusicItem{VideoID: fmt.Sprintf("p%d", i), Title: "Uno", Artist: "A", RequestedBy: "ana"}
		next := youtube.MusicItem{VideoID: fmt.Sprintf("n%d", i), Title: "Dos", RequestedBy: "bob"}
		line := radioAnnouncement(previous, next)
		if line != radioAnnouncement(previous, next) {
			t.Fatal("announcement changed for the same pair")
		}
		for _, part := range []string{"Uno", "Dos", "ana", "bob", "Radio Chatcito"} {
			if !strings.Contains(line, part) {
				t.Fatalf("%q does not contain %q", line, part)
			}
		}
		if strings.Contains(line, "%") {
			t.Fatalf("unfilled placeholder in %q", line)
		}
		seen[line] = true
	}
	if len(seen) < 15 {
		t.Errorf("only %d different announcements in 40 transitions", len(seen))
	}
}
