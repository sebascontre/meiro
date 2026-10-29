package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"

	"github.com/elianiva/meiro/internal/twitch"
	"github.com/elianiva/meiro/m3"
	"github.com/elianiva/meiro/youtube"
)

const (
	// maxPendingRequests is how many songs of one chatter may wait in the
	// queue, counting those still being looked up.
	maxPendingRequests = 3
	// maxRequestLength bounds the text searched for.
	maxRequestLength = 150
	requestTimeout   = 20 * time.Second
)

// requestCommands are the chat commands that ask for a song.
var requestCommands = []string{"!sr", "!songrequest"}

// songRequests is the reading of a Twitch chat, whose chatters add songs to
// the queue. Its fields belong to the main thread.
type songRequests struct {
	// channel is the chat being read, and cancel ends the reading; both are
	// zero while none is.
	channel   string
	cancel    context.CancelFunc
	connected bool
	// channelText is what the channel field holds, and channelError what was
	// wrong with the last one typed.
	channelText, channelError string
	channelShown              bool
	// lookups counts the searches in flight, by chatter.
	lookups map[string]int
	// find looks a song up; tests replace it.
	find func(ctx context.Context, query string) (youtube.MusicItem, error)
	// connect reads a chat until ctx ends; tests replace it.
	connect func(ctx context.Context, channel string, onMessage func(twitch.Message), onState func(bool))
}

// stop ends the reading of the chat.
func (r *songRequests) stop() {
	if r.cancel != nil {
		r.cancel()
	}
	r.cancel, r.channel, r.connected = nil, "", false
}

// syncSongRequests reads the chat the settings name, and stops reading one
// they no longer do.
func (a *app) syncSongRequests() {
	r := &a.requests
	channel := ""
	if a.settings.SongRequests {
		channel = twitch.Channel(a.settings.TwitchChannel)
	}
	if channel == r.channel {
		return
	}
	r.stop()
	if channel == "" {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.channel, r.cancel = channel, cancel
	connect := r.connect
	if connect == nil {
		connect = twitch.Run
	}
	a.run(func() {
		connect(ctx, channel, func(m twitch.Message) {
			a.update(func() {
				if ctx.Err() == nil {
					a.handleChat(m)
				}
			})
		}, func(connected bool) {
			a.update(func() {
				if ctx.Err() == nil {
					r.connected = connected
				}
			})
		})
	})
}

// requestQuery is what a chat message asks for, or "" when it asks for
// nothing.
func requestQuery(text string) string {
	command, query, _ := strings.Cut(strings.TrimSpace(text), " ")
	for _, name := range requestCommands {
		if strings.EqualFold(command, name) {
			query = strings.TrimSpace(query)
			if utf8.RuneCountInString(query) > maxRequestLength {
				return string([]rune(query)[:maxRequestLength])
			}
			return query
		}
	}
	return ""
}

// handleChat takes a message of the chat, and adds the song it asks for to
// the queue.
func (a *app) handleChat(m twitch.Message) {
	query := requestQuery(m.Text)
	if query == "" {
		return
	}
	r := &a.requests
	if r.lookups[m.User]+a.pendingRequests(m.User) >= maxPendingRequests {
		a.notice = m.User + " has " + fmt.Sprint(maxPendingRequests) + " songs waiting already"
		return
	}
	if r.lookups == nil {
		r.lookups = map[string]int{}
	}
	r.lookups[m.User]++
	find := r.find
	if find == nil {
		client := a.client()
		find = func(ctx context.Context, query string) (youtube.MusicItem, error) {
			return findSong(ctx, client, query)
		}
	}
	spawn(a, func() songResult {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		item, err := find(ctx, query)
		return songResult{item, err}
	}, func(res songResult) {
		if r.lookups[m.User]--; r.lookups[m.User] <= 0 {
			delete(r.lookups, m.User)
		}
		a.addRequest(m.User, query, res)
	})
}

type songResult struct {
	item youtube.MusicItem
	err  error
}

// pendingRequests is how many songs a chatter asked for that are playing or
// still to play.
func (a *app) pendingRequests(user string) int {
	n := 0
	for i := max(a.queue.index, 0); i < a.queue.len(); i++ {
		if a.queue.at(i).RequestedBy == user {
			n++
		}
	}
	return n
}

// addRequest queues the song found for a chatter.
func (a *app) addRequest(user, query string, res songResult) {
	if res.err != nil || res.item.VideoID == "" {
		a.notice = "No song found for " + user + "'s request: " + query
		return
	}
	item := res.item
	for i := max(a.queue.index, 0); i < a.queue.len(); i++ {
		if a.queue.at(i).VideoID == item.VideoID {
			a.notice = item.Title + " is already in the queue"
			return
		}
	}
	item.RequestedBy = user
	a.enqueue(item, false)
	a.notice = user + " requested " + item.Title
}

var videoIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// videoLinkID is the video a YouTube, YouTube Music or youtu.be link names,
// or "" when text is not such a link.
func videoLinkID(text string) string {
	if !strings.Contains(text, "/") && !strings.Contains(text, ".") {
		return ""
	}
	if !strings.Contains(text, "://") {
		text = "https://" + text
	}
	u, err := url.Parse(text)
	if err != nil {
		return ""
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	id := ""
	switch host {
	case "youtu.be":
		id = strings.Trim(u.Path, "/")
	case "youtube.com", "m.youtube.com", "music.youtube.com":
		if u.Path == "/watch" {
			id = u.Query().Get("v")
		} else if rest, ok := strings.CutPrefix(u.Path, "/shorts/"); ok {
			id = strings.Trim(rest, "/")
		}
	}
	if !videoIDPattern.MatchString(id) {
		return ""
	}
	return id
}

// findLinked is the track a link names, read from the queue YouTube Music
// builds for it, which starts with the track itself.
func findLinked(ctx context.Context, client *youtube.Client, videoID string) (youtube.MusicItem, error) {
	result, err := client.GetUpNext(ctx, youtube.UpNextOptions{VideoID: videoID})
	if err != nil {
		return youtube.MusicItem{}, err
	}
	for _, item := range result.Items {
		if item.VideoID == videoID && item.IsTrack() && !item.IsEpisode() {
			return item, nil
		}
	}
	return youtube.MusicItem{}, errors.New("no song found")
}

// findSong is the song a search of YouTube Music leads to: a song before a
// music video, and never an album, a playlist or an episode.
func findSong(ctx context.Context, client *youtube.Client, query string) (youtube.MusicItem, error) {
	if id := videoLinkID(query); id != "" {
		return findLinked(ctx, client, id)
	}
	result, err := client.Search(ctx, query, youtube.SearchOptions{})
	if err != nil {
		return youtube.MusicItem{}, err
	}
	var video youtube.MusicItem
	for _, item := range append([]youtube.MusicItem{result.TopResult}, result.Items...) {
		if !item.IsTrack() || item.IsEpisode() {
			continue
		}
		if !item.IsVideo() {
			return item, nil
		}
		if video.VideoID == "" {
			video = item
		}
	}
	if video.VideoID == "" {
		return video, errors.New("no song found")
	}
	return video, nil
}

// songRequestsCard is where the channel whose chat asks for songs is set.
func (a *app) songRequestsCard(c *ui.Context) {
	sc := m3.Of(c).Scheme
	a.settingsCard(c, "Song requests", m3.IconMusicNote, func() {
		ui.Column(c).Gap(12).Children(func() {
			ui.Row(c).Gap(16).AlignItems(ui.Center).Children(func() {
				settingLabel(c, "Twitch song requests", "Chatters of the channel add songs to the end of the queue with !sr followed by a song and artist, or a YouTube link, and the queue shows who asked. Meiro only reads the chat; it does not sign in or reply.").Grow(1)
				on := a.settings.SongRequests
				if m3.Switch(c, &on, "Twitch song requests").Changed() {
					a.settings.SongRequests = on
					a.syncSongRequests()
					a.saveSettings()
				}
			})
			if !a.requests.channelShown {
				a.requests.channelText, a.requests.channelShown = a.settings.TwitchChannel, true
			}
			settingLabel(c, "Channel", "The name of the channel, or its link. Press Enter to apply.")
			input := ui.TextInputBase(c.Key("twitch-channel"), &a.requests.channelText).
				Width(280).Height(48).Shrink(0).Padding(12).Radius(m3.Medium).Background(sc.SurfaceContainerHigh).
				TextColor(sc.OnSurface).FontSize(16).Label("Twitch channel").Placeholder("channel name")
			if input.Submitted() {
				a.setTwitchChannel(a.requests.channelText)
			}
			switch {
			case !a.settings.SongRequests:
			case twitch.Channel(a.settings.TwitchChannel) == "":
				m3.Text(c, m3.BodyMedium, "Enter a channel to read.").TextColor(sc.OnSurfaceVariant)
			case a.requests.connected:
				m3.Text(c, m3.BodyMedium, "Listening to "+twitch.Channel(a.settings.TwitchChannel)+"'s chat.").TextColor(sc.OnSurfaceVariant)
			default:
				m3.Text(c, m3.BodyMedium, "Connecting to the chat…").TextColor(sc.OnSurfaceVariant)
			}
			if a.requests.channelError != "" {
				m3.Text(c, m3.BodyMedium, a.requests.channelError).TextColor(sc.Error)
			}
		})
	})
}

// setTwitchChannel keeps the channel the user typed, and reads its chat when
// song requests are on.
func (a *app) setTwitchChannel(text string) {
	if strings.TrimSpace(text) == "" {
		a.settings.TwitchChannel, a.requests.channelError = "", ""
	} else if channel := twitch.Channel(text); channel == "" {
		a.requests.channelError = "That is not a Twitch channel name."
		return
	} else {
		a.settings.TwitchChannel, a.requests.channelText, a.requests.channelError = channel, channel, ""
	}
	a.syncSongRequests()
	a.saveSettings()
}
