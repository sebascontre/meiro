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
	"slices"
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
		"!sr_id dQw4w9WgXcQ":             "",
		"!sr":                            "",
		"hello !sr x":                    "",
		"!srx foo":                       "",
	} {
		if got := requestQuery(in); got != want {
			t.Errorf("requestQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRequestVideoID(t *testing.T) {
	for in, want := range map[string]string{
		"!sr_id dQw4w9WgXcQ":   "dQw4w9WgXcQ",
		"!SR_ID  dQw4w9WgXcQ ": "dQw4w9WgXcQ",
		"!sr_id":               "",
		"!sr_id too-short":     "",
		"!sr dQw4w9WgXcQ":      "",
	} {
		got, handled := requestVideoID(in)
		if got != want || (in != "!sr dQw4w9WgXcQ" && !handled) ||
			(in == "!sr dQw4w9WgXcQ" && handled) {
			t.Errorf("requestVideoID(%q) = %q, %v; want %q", in, got, handled, want)
		}
	}
}

func TestSRIDLooksUpExactVideoAndQueuesIt(t *testing.T) {
	a := newTestApp(t)
	a.current = youtube.MusicItem{VideoID: "current", Title: "Now"}
	a.queue.items, a.queue.index = []youtube.MusicItem{a.current}, 0
	var lookedUp string
	a.requests.findID = func(_ context.Context, id string) (youtube.MusicItem, error) {
		lookedUp = id
		return youtube.MusicItem{VideoID: id, Title: "Direct song", Kind: youtube.KindTrack}, nil
	}

	a.handleChat(twitch.Message{User: "ana", Text: "!sr_id dQw4w9WgXcQ"})

	if lookedUp != "dQw4w9WgXcQ" || a.queue.len() != 2 ||
		a.queue.at(1).VideoID != lookedUp || a.queue.at(1).RequestedBy != "ana" {
		t.Fatalf("looked up %q, queue=%v", lookedUp, ids(a.queue.items))
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
	if n := a.pendingRequests("ana"); n != defaultPendingRequests {
		t.Errorf("pending = %d, want %d", n, defaultPendingRequests)
	}
	if len(replies) != 4 || !strings.Contains(replies[2], "posición 4") ||
		!strings.Contains(replies[3], "límite de 3") {
		t.Errorf("replies = %v, want third song at position 4 and limit notice", replies)
	}
}

func TestSongRequestLimitIsConfigurableAndRepliesAtTheCap(t *testing.T) {
	a := newTestApp(t)
	a.settings.SongRequestLimit = 1
	a.requests.token = "validated"
	var replies []string
	a.requests.sendChat = func(message string) error {
		replies = append(replies, message)
		return nil
	}
	a.requests.find = func(_ context.Context, query string) (youtube.MusicItem, error) {
		return youtube.MusicItem{VideoID: query, Title: query, Kind: youtube.KindTrack}, nil
	}
	a.current = youtube.MusicItem{VideoID: "current"}
	a.queue.items, a.queue.index = []youtube.MusicItem{a.current}, 0

	a.handleChat(twitch.Message{User: "ana", Text: "!sr first"})
	a.handleChat(twitch.Message{User: "ana", Text: "!sr second"})
	if a.pendingRequestLimit() != 1 || a.pendingRequests("ana") != 1 {
		t.Fatalf("limit=%d pending=%d", a.pendingRequestLimit(), a.pendingRequests("ana"))
	}
	if len(replies) != 2 || !strings.Contains(replies[1], "límite de 1") {
		t.Fatalf("replies = %v, want cap notice", replies)
	}
	a.settings.SongRequestLimit = 5
	if a.pendingRequestLimit() != 5 {
		t.Fatalf("configured limit = %d, want 5", a.pendingRequestLimit())
	}
}

func TestSRErrorRemovesTheMostRecentUnplayedRequest(t *testing.T) {
	a := newTestApp(t)
	a.settings.SongRequests = true
	a.requests.token = "validated"
	var replies []string
	a.requests.sendChat = func(message string) error {
		replies = append(replies, message)
		return nil
	}
	a.current = youtube.MusicItem{VideoID: "current", Title: "Now"}
	a.queue.items = []youtube.MusicItem{
		a.current,
		{VideoID: "ana-first", Title: "First", RequestedBy: "ana"},
		{VideoID: "bob", Title: "Bob's song", RequestedBy: "bob"},
		{VideoID: "ana-last", Title: "Last", RequestedBy: "ana"},
	}
	a.queue.index = 0

	a.handleChat(twitch.Message{User: "ANA", Text: "!sr_error"})

	if got, want := ids(a.queue.items), []string{"current", "ana-first", "bob"}; !slices.Equal(got, want) {
		t.Fatalf("queue after undo = %v, want %v", got, want)
	}
	if len(replies) != 1 || !strings.Contains(replies[0], "Last") {
		t.Fatalf("replies = %v, want removal confirmation for Last", replies)
	}
}

func TestSRErrorReportsWhenThereIsNothingToRemove(t *testing.T) {
	a := newTestApp(t)
	a.requests.token = "validated"
	var replies []string
	a.requests.sendChat = func(message string) error {
		replies = append(replies, message)
		return nil
	}
	a.current = youtube.MusicItem{VideoID: "current", RequestedBy: "ana"}
	a.queue.items, a.queue.index = []youtube.MusicItem{a.current}, 0

	a.handleChat(twitch.Message{User: "ana", Text: "!Sr_Error"})

	if a.queue.len() != 1 || len(replies) != 1 || !strings.Contains(replies[0], "no tienes canciones pendientes") {
		t.Fatalf("queue=%v replies=%v", ids(a.queue.items), replies)
	}
}

func TestSongRequestPromotesAnAutoplayTrackIntoTheRequestedQueue(t *testing.T) {
	a := newTestApp(t)
	a.settings.AutoPlay = true
	current := youtube.MusicItem{VideoID: "current", Title: "Now", Kind: youtube.KindTrack}
	a.current = current
	a.queue.items = []youtube.MusicItem{
		current,
		{VideoID: "other", Title: "Other", Kind: youtube.KindTrack},
		{VideoID: "requested", Title: "Requested song", Kind: youtube.KindTrack},
		{VideoID: "last", Title: "Last", Kind: youtube.KindTrack},
	}
	a.queue.index, a.queue.recStart = 0, 1
	a.queue.generated = map[string]struct{}{"other": {}, "requested": {}, "last": {}}

	a.addRequest("ana", "requested song", songResult{item: youtube.MusicItem{
		VideoID: "requested", Title: "Requested song", Kind: youtube.KindTrack,
	}})

	want := []string{"current", "requested", "other", "last"}
	if got := ids(a.queue.items); !slices.Equal(got, want) {
		t.Fatalf("queue = %v, want %v", got, want)
	}
	if a.queue.at(1).RequestedBy != "ana" {
		t.Fatalf("promoted song requester = %q", a.queue.at(1).RequestedBy)
	}
	if a.queue.recStart != 2 {
		t.Fatalf("autoplay starts at %d, want 2", a.queue.recStart)
	}
	if _, generated := a.queue.generated["requested"]; generated {
		t.Fatal("promoted song is still marked as generated")
	}
	if next, ok := a.queue.next(true, repeatOff); !ok || a.queue.at(next).VideoID != "requested" {
		t.Fatalf("next = %v, %v; want requested song", next, ok)
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
			Voice struct{ Name, LanguageCode string }
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Input.Text != "hola" || body.Voice.Name != "es-ES-Chirp3-HD-Kore" || body.Voice.LanguageCode != "es-ES" {
			t.Errorf("request = %+v", body)
		}
		_, _ = w.Write([]byte(`{"audioContent":"` + base64.StdEncoding.EncodeToString([]byte("mp3")) + `"}`))
	}))
	defer server.Close()
	audio, err := googleSpeech(context.Background(), server.URL, "k", "es-ES-Chirp3-HD-Kore", "hola")
	if err != nil || string(audio) != "mp3" {
		t.Fatalf("audio %q, err %v", audio, err)
	}
	if _, err := googleSpeech(context.Background(), server.URL, "bad", "", "hola"); err == nil || !strings.Contains(err.Error(), "API key not valid") {
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

func TestRadioWordsSkipsParentheses(t *testing.T) {
	for in, want := range map[string]string{
		"Get Lucky (feat. Pharrell Williams)": "Get Lucky",
		"One (Remastered (2011)) More Time":   "One More Time",
		"(Intro) Hola":                        "Hola",
		"Sin cerrar (nota":                    "Sin cerrar",
		"Cierre ) suelto":                     "Cierre suelto",
		"Normal":                              "Normal",
		"Ana_Perez":                           "Ana Perez",
		"OSHI NO KO":                          "oshi no ko",
		"YOASOBI - IDOL (Live)":               "yoasobi - idol",
		"Hello WORLD I am A DJ":               "Hello world I am A dj",
		"Mixed CASE Stays":                    "Mixed case Stays",
	} {
		if got := radioWords(in); got != want {
			t.Errorf("radioWords(%q) = %q, want %q", in, got, want)
		}

	}
	line := radioAnnouncement(
		youtube.MusicItem{VideoID: "a", Title: "Uno (Live)", Artist: "Band (Official)", RequestedBy: "ana"},
		youtube.MusicItem{VideoID: "b", Title: "Dos (Remix)", RequestedBy: "bob"})
	if strings.ContainsAny(line, "()") || strings.Contains(line, "Live") || strings.Contains(line, "Remix") {
		t.Errorf("announcement reads parenthesised text: %q", line)
	}
}

func TestRadioTitlePrefersJapaneseBeforeLatinDashSuffix(t *testing.T) {
	for in, want := range map[string]string{
		"アイドル - YOASOBI":        "アイドル",
		"夜に駆ける — YOASOBI":       "夜に駆ける",
		"Song - Artist":         "Song - Artist",
		"アイドル - 夜に駆ける":          "アイドル - 夜に駆ける",
		"アイドル (Live) - YOASOBI": "アイドル",
	} {
		if got := radioTitle(in); got != want {
			t.Errorf("radioTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRadioArtistExpandsItsYamiVT(t *testing.T) {
	for _, artist := range []string{"itsyamivt", "ItsYamiVT", " ITSYAMIVT "} {
		if got := radioArtist(artist); got != "It´s Yami VT" {
			t.Errorf("radioArtist(%q) = %q", artist, got)
		}
	}
	if got := radioArtist("Other Artist"); got != "Other Artist" {
		t.Errorf("radioArtist changed unrelated artist to %q", got)
	}
}

func TestGoogleVoicesListsSpanishChirpVoices(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"voices":[
			{"name":"es-US-Chirp3-HD-Puck"},{"name":"en-US-Chirp3-HD-Puck"},{"name":"es-US-Neural2-B"},
			{"name":"es-ES-Chirp3-HD-Aoede"},{"name":"es-US-Chirp3-HD-Aoede"}]}`))
	}))
	defer server.Close()
	voices, err := googleVoices(context.Background(), server.URL, "k")
	want := []string{"es-ES-Chirp3-HD-Aoede", "es-US-Chirp3-HD-Aoede", "es-US-Chirp3-HD-Puck"}
	if err != nil || !slices.Equal(voices, want) {
		t.Fatalf("voices = %v, err %v", voices, err)
	}
	if got := voiceLabel("es-US-Chirp3-HD-Puck"); got != "Puck (es-US)" {
		t.Errorf("label = %q", got)
	}
	if got := voiceLanguage("es-ES-Chirp3-HD-Kore"); got != "es-ES" {
		t.Errorf("language = %q", got)
	}
}
