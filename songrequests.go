package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"

	"github.com/elianiva/meiro/internal/secretfile"
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
	channel         string
	connectionLogin string
	cancel          context.CancelFunc
	connected       bool
	// token and login are the validated Twitch credentials in memory.
	token, login string
	// sendChat is available only while the authenticated connection is joined.
	sendChat func(string) error
	// pendingReplies waits for an authenticated reconnect, in order.
	pendingReplies []string
	radioPair      string
	radioChosen    bool
	// tokenText exists only in the settings field; tokenError says why the
	// last attempt did not connect.
	tokenText, tokenError string
	tokenStore            secretfile.File
	tokenTask             task
	tokenLoad             task
	radio                 task
	radioRoll             func() bool
	speakRadio            func(context.Context, string, string) error
	// clipLine is the announcement whose audio is fetched ahead, kept in
	// clipPath once ready; clipLoad is the fetch in flight.
	clipLine, clipPath string
	clipLoad           task
	fetchClip          func(context.Context, string) (string, error)
	// ttsKey is the Google Cloud Text-to-Speech API key; without one Radio
	// Chatcito stays silent.
	ttsKey, ttsText, ttsError string
	ttsStore                  secretfile.File
	ttsLoad                   task
	ttsTest                   task
	ttsStatus                 string
	// channelText is what the channel field holds, and channelError what was
	// wrong with the last one typed.
	channelText, channelError string
	channelShown              bool
	// lookups counts the searches in flight, by chatter.
	lookups map[string]int
	// find looks a song up; tests replace it.
	find func(ctx context.Context, query string) (youtube.MusicItem, error)
	// connect reads a chat until ctx ends; tests replace it.
	connect              func(ctx context.Context, channel string, onMessage func(twitch.Message), onState func(bool))
	connectAuthenticated func(ctx context.Context, channel, login, token string, onMessage func(twitch.Message), onState func(bool), onSend func(func(string) error))
}

// stop ends the reading of the chat.
func (r *songRequests) stop() {
	if r.cancel != nil {
		r.cancel()
	}
	r.cancel, r.channel, r.connectionLogin, r.connected, r.sendChat, r.pendingReplies = nil, "", "", false, nil, nil
	clear(r.lookups)
}

// syncSongRequests reads the chat the settings name, and stops reading one
// they no longer do.
func (a *app) syncSongRequests() {
	r := &a.requests
	channel := ""
	if a.settings.SongRequests {
		channel = twitch.Channel(a.settings.TwitchChannel)
	} else {
		r.radio.stop()
		r.radioPair, r.radioChosen = "", false
	}
	a.syncPreload()
	login := ""
	if r.token != "" {
		login = r.login
	}
	if channel == r.channel && login == r.connectionLogin {
		return
	}
	r.stop()
	if channel == "" {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.channel, r.connectionLogin, r.cancel = channel, login, cancel
	token := r.token
	username := r.login
	connect := r.connect
	connectAuthenticated := r.connectAuthenticated
	if token != "" {
		if connectAuthenticated == nil {
			connectAuthenticated = twitch.RunAuthenticated
		}
		a.run(func() {
			connectAuthenticated(ctx, channel, username, token, func(m twitch.Message) {
				a.update(func() {
					if ctx.Err() == nil {
						a.handleChatFrom(ctx, m)
					}
				})
			}, func(connected bool) {
				a.update(func() {
					if ctx.Err() == nil {
						r.connected = connected
					}
				})
			}, func(send func(string) error) {
				a.update(func() {
					if ctx.Err() == nil {
						r.sendChat = send
						if send != nil {
							pending := r.pendingReplies
							r.pendingReplies = nil
							for _, message := range pending {
								a.replyInChat(message)
							}
						}
					}
				})
			})
		})
		return
	}
	if connect == nil {
		connect = twitch.Run
	}
	a.run(func() {
		connect(ctx, channel, func(m twitch.Message) {
			a.update(func() {
				if ctx.Err() == nil {
					a.handleChatFrom(ctx, m)
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

// radioTransitionChance is one announcer break in this many eligible
// transitions on average.
const radioTransitionChance = 4

// radioScripts are the announcer's lines; %P is the song that just played
// and %N the one that follows. Every one names Radio Chatcito.
var radioScripts = []string{
	"Acabamos de escuchar %P. Seguimos aquí, en Radio Chatcito. Ahora escuchamos %N. ¡Gracias por acompañarnos!",
	"Y así sonó %P. Esto es Radio Chatcito, donde el chat manda. Sigue %N. ¡Que lo disfruten!",
	"¡Qué tema! Eso fue %P. Pero no se muevan, que en Radio Chatcito la música no para: ahora viene %N.",
	"Radio Chatcito, la radio de ustedes. Hace un momento sonaba %P. Y ahora, atentos, que llega %N.",
	"Eso fue %P. Gracias por seguir sintonizados en Radio Chatcito. A continuación, %N.",
	"Se acabó %P, pero la fiesta sigue aquí, en Radio Chatcito. Prepárense, porque ahora suena %N.",
	"Ustedes piden, nosotros ponemos. Acaba de sonar %P, y ahora, en Radio Chatcito, %N.",
	"Seguimos con ustedes en Radio Chatcito. Antes escuchamos %P, y ahora subimos el ánimo con %N.",
	"Si están sintonizando Radio Chatcito, llegaron en buen momento. Terminó %P y empieza %N. ¡Disfrútenlo!",
	"Qué bueno que nos acompañan en Radio Chatcito. Dejamos atrás %P y nos vamos con %N.",
}

var radioRequestPhrases = []string{"pedida por %U", "a pedido de %U", "petición de %U", "que pidió %U"}

// radioPick chooses from n options by a hash of the pair of songs and a salt,
// so the same transition always says the same thing, and different ones vary.
func radioPick(previous, next youtube.MusicItem, salt string, n int) int {
	sum := sha256.Sum256([]byte(salt + "#" + previous.VideoID + ">" + next.VideoID))
	return int(binary.BigEndian.Uint32(sum[:4]) % uint32(n))
}

func radioDescribe(item youtube.MusicItem, title string, phrase int) string {
	text := "«" + title + "»"
	if artist := radioWords(item.Artist); artist != "" {
		text += ", de " + artist
	}
	if user := radioWords(item.RequestedBy); user != "" {
		text += ", " + strings.ReplaceAll(radioRequestPhrases[phrase], "%U", user)
	}
	return text
}

// radioAnnouncement is a short Spanish link between two songs. It varies from
// one transition to another but is stable for the same pair.
func radioAnnouncement(previous, next youtube.MusicItem) string {
	previousTitle, nextTitle := radioWords(previous.Title), radioWords(next.Title)
	if previousTitle == "" || nextTitle == "" {
		return ""
	}
	script := radioScripts[radioPick(previous, next, "script", len(radioScripts))]
	script = strings.Replace(script, "%P", radioDescribe(previous, previousTitle, radioPick(previous, next, "previous", len(radioRequestPhrases))), 1)
	return strings.Replace(script, "%N", radioDescribe(next, nextTitle, radioPick(previous, next, "next", len(radioRequestPhrases))), 1)
}

func radioWords(text string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(text) {
		if r < ' ' || r == 0x7f {
			continue
		}
		if b.Len() >= 160 {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

func requestedSong(item youtube.MusicItem) bool {
	return item.RequestedBy != "" && item.IsTrack() && !item.IsEpisode()
}

// radioTransition schedules an occasional spoken link after the current song
// ends and before the next request begins.
func (a *app) radioTransition(previous, next youtube.MusicItem, nextIndex int) bool {
	if !(a.player.Ended() || a.endingEarly) || !a.radioPlanned(nextIndex, next) {
		return false
	}
	line := radioAnnouncement(previous, next)
	if line == "" {
		return false
	}
	currentID, currentIndex, nextID := previous.VideoID, a.queue.index, next.VideoID
	clip := ""
	if a.requests.clipLine == line {
		clip, a.requests.clipPath = a.requests.clipPath, ""
	}
	a.discardRadioClip()
	speak := a.requests.speakRadio
	if speak == nil {
		speak = a.speakRadio
	}
	startTask(a, &a.requests.radio, 45*time.Second, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, speak(ctx, line, clip)
	}, func(_ struct{}, err error) {
		if err != nil {
			log.Printf("speaking Twitch song request transition: %v", err)
			a.requests.ttsStatus = "Last narration failed: " + err.Error()
		} else {
			a.requests.ttsStatus = ""
		}
		if !a.settings.SongRequests || a.current.VideoID != currentID || a.queue.index != currentIndex ||
			nextIndex >= a.queue.len() || a.queue.at(nextIndex).VideoID != nextID {
			return
		}
		a.startNext(a.queue.moveTo(nextIndex))
	})
	return true
}

// radioPrefetchLead is how long before a song ends its announcement is fetched.
const radioPrefetchLead = 45 * time.Second

// discardRadioClip drops any announcement audio fetched ahead.
func (a *app) discardRadioClip() {
	a.requests.clipLoad.stop()
	if a.requests.clipPath != "" {
		_ = os.Remove(a.requests.clipPath)
	}
	a.requests.clipLine, a.requests.clipPath = "", ""
}

// prefetchRadio fetches the announcement audio shortly before the song ends,
// so the break starts without waiting on the network.
func (a *app) prefetchRadio(next youtube.MusicItem) {
	line := radioAnnouncement(a.current, next)
	if line == "" || a.requests.speakRadio != nil && a.requests.fetchClip == nil {
		return
	}
	if a.requests.clipLine != line {
		a.discardRadioClip()
	}
	if a.requests.clipLine == line || a.total <= 0 || !a.player.Playing() || a.total-a.player.Position() > radioPrefetchLead {
		return
	}
	a.requests.clipLine = line
	fetch := a.requests.fetchClip
	if fetch == nil {
		key := a.requests.ttsKey
		fetch = func(ctx context.Context, line string) (string, error) { return fetchRadioClip(ctx, key, line) }
	}
	startTask(a, &a.requests.clipLoad, 20*time.Second, func(ctx context.Context) (string, error) {
		return fetch(ctx, line)
	}, func(path string, err error) {
		if err != nil {
			log.Printf("fetching the radio voice ahead of time: %v", err)
			a.requests.ttsStatus = "Last narration failed: " + err.Error()
			return
		}
		if a.requests.clipLine != line {
			_ = os.Remove(path)
			return
		}
		a.requests.clipPath = path
	})
}

func (a *app) radioPlanned(nextIndex int, next youtube.MusicItem) bool {
	if !a.settings.SongRequests || nextIndex < 0 || a.requests.ttsKey == "" {
		a.requests.radioPair, a.requests.radioChosen = "", false
		return false
	}
	if a.settings.RadioAlways {
		return true
	}
	if !requestedSong(a.current) || !requestedSong(next) {
		a.requests.radioPair, a.requests.radioChosen = "", false
		return false
	}
	pair := fmt.Sprintf("%s:%d>%s:%d", a.current.VideoID, a.queue.index, next.VideoID, nextIndex)
	if pair != a.requests.radioPair {
		a.requests.radioPair = pair
		if a.requests.radioRoll != nil {
			a.requests.radioChosen = a.requests.radioRoll()
		} else {
			a.requests.radioChosen = rand.IntN(radioTransitionChance) == 0
		}
	}
	return a.requests.radioChosen
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
	a.handleChatFrom(context.Background(), m)
}

func (a *app) handleChatFrom(source context.Context, m twitch.Message) {
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
		if source.Err() != nil {
			return
		}
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
	if position := requestedPosition(&a.queue, item.VideoID, user); position > 0 && a.requests.token != "" {
		a.replyInChat(fmt.Sprintf("@%s: %q fue agregada en la posición %d de la cola (la canción actual cuenta como posición 1).",
			user, item.Title, position))
	}
}

// requestedPosition is the one-based place after the currently playing track,
// which itself is position 1.
func requestedPosition(q *queue, videoID, user string) int {
	for index := max(q.index, 0); index < q.len(); index++ {
		if item := q.at(index); item.VideoID == videoID && item.RequestedBy == user {
			return index - q.index + 1
		}
	}
	return 0
}

// replyInChat sends a confirmation off the UI thread when the authenticated
// chat connection is ready.
func (a *app) replyInChat(message string) {
	r := &a.requests
	send := r.sendChat
	if send == nil {
		if r.token != "" {
			if len(r.pendingReplies) < 25 {
				r.pendingReplies = append(r.pendingReplies, message)
			} else {
				log.Printf("dropping Twitch chat reply: outgoing reply queue is full")
			}
		}
		return
	}
	a.run(func() {
		if err := send(message); err != nil {
			log.Printf("sending Twitch song request confirmation: %v", err)
			a.update(func() { a.notice = "Song added, but Twitch could not send the chat reply." })
		}
	})
}

// loadTwitchToken reads the saved token and checks it before enabling replies.
func (a *app) loadTwitchToken(store secretfile.File) {
	r := &a.requests
	r.tokenStore = store
	startTask(a, &r.tokenLoad, 30*time.Second, func(ctx context.Context) (string, error) {
		return store.Load(ctx)
	}, func(token string, err error) {
		if errors.Is(err, secretfile.ErrNotSaved) {
			return
		}
		if err != nil {
			log.Printf("reading Twitch credentials: %v", err)
			r.tokenError = "Could not read the saved Twitch token."
			return
		}
		a.authorizeTwitchToken(token, false)
	})
}

// authorizeTwitchToken validates a token before saving it or using it to join
// chat. Saving happens off the UI thread, in the system credential store.
func (a *app) authorizeTwitchToken(token string, save bool) {
	token = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(token), "oauth:"))
	if token == "" {
		a.requests.tokenError = "Paste a Twitch access token."
		return
	}
	r := &a.requests
	startTask(a, &r.tokenTask, 30*time.Second, func(ctx context.Context) (string, error) {
		login, err := twitch.Validate(ctx, token)
		if err != nil {
			return "", err
		}
		if save {
			if r.tokenStore.Path == "" {
				return "", errors.New("Twitch token storage is not available")
			}
			if err := r.tokenStore.Save(ctx, token); err != nil {
				return "", fmt.Errorf("saving Twitch token: %w", err)
			}
		}
		return login, nil
	}, func(login string, err error) {
		if err != nil {
			log.Printf("connecting Twitch chat: %v", err)
			r.tokenError = err.Error()
			return
		}
		r.token, r.login, r.tokenText, r.tokenError = token, login, "", ""
		a.syncSongRequests()
	})
}

// loadTTSKey reads the saved Google Text-to-Speech key.
func (a *app) loadTTSKey(store secretfile.File) {
	r := &a.requests
	r.ttsStore = store
	startTask(a, &r.ttsLoad, 30*time.Second, func(ctx context.Context) (string, error) {
		return store.Load(ctx)
	}, func(key string, err error) {
		if errors.Is(err, secretfile.ErrNotSaved) {
			return
		}
		if err != nil {
			log.Printf("reading the Text-to-Speech key: %v", err)
			r.ttsError = "Could not read the saved Text-to-Speech key."
			return
		}
		r.ttsKey = key
		a.syncPreload()
	})
}

// testRadioVoice speaks a sample line, so a wrong key or a disabled API shows
// up in Settings instead of on the air.
func (a *app) testRadioVoice() {
	r := &a.requests
	key := r.ttsKey
	r.ttsStatus = "Testing the voice…"
	speak := r.speakRadio
	if speak == nil {
		speak = a.speakRadio
	}
	startTask(a, &r.ttsTest, 45*time.Second, func(ctx context.Context) (struct{}, error) {
		if key == "" {
			return struct{}{}, errors.New("save a Google API key first")
		}
		return struct{}{}, speak(ctx, "Esto es Radio Chatcito. La voz funciona perfectamente.", "")
	}, func(_ struct{}, err error) {
		if err != nil {
			log.Printf("testing the radio voice: %v", err)
			r.ttsStatus = "Voice test failed: " + err.Error()
			return
		}
		r.ttsStatus = "Voice test played."
	})
}

// saveTTSKey keeps a pasted key, or removes the saved one when it is empty.
func (a *app) saveTTSKey(text string) {
	r := &a.requests
	key := cleanSpeechKey(text)
	r.ttsKey, r.ttsText, r.ttsError = key, "", ""
	a.discardRadioClip()
	a.syncPreload()
	store := r.ttsStore
	if store.Path == "" {
		return
	}
	spawn(a, func() error {
		if key == "" {
			return store.Delete(context.Background())
		}
		return store.Save(context.Background(), key)
	}, func(err error) {
		if err != nil {
			log.Printf("storing the Text-to-Speech key: %v", err)
			r.ttsError = "Could not store the Text-to-Speech key."
		}
	})
}

// disconnectTwitch clears the saved sending credential. Song Request itself
// remains on, and can continue reading chat anonymously.
func (a *app) disconnectTwitch() {
	r := &a.requests
	r.tokenTask.stop()
	r.token, r.login, r.sendChat = "", "", nil
	a.syncSongRequests()
	store := r.tokenStore
	spawn(a, func() error { return store.Delete(context.Background()) }, func(err error) {
		if err != nil {
			log.Printf("removing Twitch credentials: %v", err)
			r.tokenError = "Could not remove the saved Twitch token."
			return
		}
		r.tokenError = ""
	})
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

// findSong is the song a search of YouTube Music leads to. It looks in the
// search's Songs category first, which holds the official recordings, and
// only falls back to the other results, ranked, when that has none.
func findSong(ctx context.Context, client *youtube.Client, query string) (youtube.MusicItem, error) {
	if id := videoLinkID(query); id != "" {
		return findLinked(ctx, client, id)
	}
	result, err := client.Search(ctx, query, youtube.SearchOptions{})
	if err != nil {
		return youtube.MusicItem{}, err
	}
	for _, filter := range result.Filters {
		if strings.EqualFold(filter.Label, "Songs") && filter.Params != "" {
			songs, err := client.Search(ctx, query, youtube.SearchOptions{Params: filter.Params})
			if err == nil {
				if item, ok := pickSong(songs.Items); ok {
					return item, nil
				}
			}
			break
		}
	}
	if item, ok := pickSong(append([]youtube.MusicItem{result.TopResult}, result.Items...)); ok {
		return item, nil
	}
	return youtube.MusicItem{}, errors.New("no song found")
}

// unofficialWords mark a version that is not the recording asked for.
var unofficialWords = regexp.MustCompile(`(?i)\b(live|cover|remix|karaoke|instrumental|nightcore|slowed|reverb|sped up|speed up|8d|1 hour|10 hours?|loop|reaction|tutorial|lesson|mashup|bass boosted)\b`)

// maxRequestDuration is the longest track a request may add, which rules out
// hour-long loops and mixes.
const maxRequestDuration = 12 * time.Minute

// pickSong is the track to queue among search results. Songs come before
// videos, songs and videos with the marks of a copy come last, and the order
// YouTube gave breaks ties.
func pickSong(items []youtube.MusicItem) (youtube.MusicItem, bool) {
	best, bestScore := youtube.MusicItem{}, -1000
	for i, item := range items {
		if !item.IsTrack() || item.IsEpisode() {
			continue
		}
		score := -i
		if !item.IsVideo() {
			score += 100
		}
		if strings.Contains(strings.ToLower(item.Title), "official") {
			score += 20
		}
		if unofficialWords.MatchString(item.Title) {
			score -= 60
		}
		if d := parseClock(item.Duration); d > maxRequestDuration {
			score -= 200
		}
		if score > bestScore {
			best, bestScore = item, score
		}
	}
	if best.VideoID == "" || bestScore < -150 {
		return youtube.MusicItem{}, false
	}
	return best, true
}

// parseClock reads a length such as "4:09" or "1:02:03", or returns 0.
func parseClock(text string) time.Duration {
	total := time.Duration(0)
	for _, part := range strings.Split(strings.TrimSpace(text), ":") {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return 0
		}
		total = total*60 + time.Duration(n)*time.Second
	}
	return total
}

// songRequestsCard is where the channel whose chat asks for songs is set.
func (a *app) songRequestsCard(c *ui.Context) {
	sc := m3.Of(c).Scheme
	a.settingsCard(c, "Song requests", m3.IconMusicNote, func() {
		ui.Column(c).Gap(12).Children(func() {
			ui.Row(c).Gap(16).AlignItems(ui.Center).Children(func() {
				settingLabel(c, "Twitch song requests", "Chatters add songs with !sr followed by a song and artist, or a YouTube link. Meiro shows who requested each song. Add a Twitch token below to confirm successful requests in chat with their queue position.").Grow(1)
				on := a.settings.SongRequests
				if m3.Switch(c, &on, "Twitch song requests").Changed() {
					a.settings.SongRequests = on
					a.syncSongRequests()
					a.saveSettings()
				}
			})
			ui.Row(c).Gap(16).AlignItems(ui.Center).Children(func() {
				settingLabel(c, "Always narrate (testing)", "Radio Chatcito speaks between every pair of songs, requested or not, instead of one in four requested transitions.").Grow(1)
				always := a.settings.RadioAlways
				if m3.Switch(c, &always, "Always narrate").Changed() {
					a.settings.RadioAlways = always
					a.saveSettings()
					a.syncPreload()
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
			ui.Column(c).Gap(8).Children(func() {
				settingLabel(c, "Chat replies", "Paste a Twitch access token with chat:read and chat:edit permissions. Meiro validates it and stores it separately from Settings, encrypted when the system has a credential store.")
				if a.requests.login != "" {
					ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
						m3.Text(c, m3.BodyMedium, "Sending as "+a.requests.login).TextColor(sc.OnSurfaceVariant).Grow(1)
						if m3.Button(c, m3.ButtonSpec{Label: "Disconnect", Kind: m3.Tonal, Size: m3.Medium56, Key: "twitch-disconnect"}).Clicked() {
							a.disconnectTwitch()
						}
					})
				} else {
					ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
						token := ui.TextInputBase(c.Key("twitch-token"), &a.requests.tokenText).
							Width(360).Height(48).Shrink(1).Padding(12).Radius(m3.Medium).Background(sc.SurfaceContainerHigh).
							TextColor(sc.OnSurface).FontSize(16).Label("Twitch access token").Placeholder("access token").Password()
						if token.Submitted() {
							a.authorizeTwitchToken(a.requests.tokenText, true)
						}
						if m3.Button(c, m3.ButtonSpec{Label: "Connect", Kind: m3.Filled, Size: m3.Medium56, Disabled: a.requests.tokenTask.busy(), Key: "twitch-connect"}).Clicked() {
							a.authorizeTwitchToken(a.requests.tokenText, true)
						}
					})
				}
				if a.requests.tokenTask.busy() || a.requests.tokenLoad.busy() {
					m3.Text(c, m3.BodyMedium, "Checking Twitch access…").TextColor(sc.OnSurfaceVariant)
				}
				if a.requests.tokenError != "" {
					m3.Text(c, m3.BodyMedium, a.requests.tokenError).TextColor(sc.Error).MaxLines(3)
				}
			})
			ui.Column(c).Gap(8).Children(func() {
				settingLabel(c, "Radio Chatcito voice", "Paste a Google Cloud Text-to-Speech API key. Radio Chatcito will occasionally introduce a requested song before the next, speaking only between songs, and stays silent without a key.")
				if a.requests.ttsKey != "" {
					ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
						m3.Text(c, m3.BodyMedium, "Google voice key saved").TextColor(sc.OnSurfaceVariant).Grow(1)
						if m3.Button(c, m3.ButtonSpec{Label: "Remove", Kind: m3.Tonal, Size: m3.Medium56, Key: "tts-remove"}).Clicked() {
							a.saveTTSKey("")
						}
						if m3.Button(c, m3.ButtonSpec{Label: "Test voice", Kind: m3.Filled, Size: m3.Medium56, Disabled: a.requests.ttsTest.busy(), Key: "tts-test"}).Clicked() {
							a.testRadioVoice()
						}
					})
				} else {
					ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
						key := ui.TextInputBase(c.Key("tts-key"), &a.requests.ttsText).
							Width(360).Height(48).Shrink(1).Padding(12).Radius(m3.Medium).Background(sc.SurfaceContainerHigh).
							TextColor(sc.OnSurface).FontSize(16).Label("Google API key").Placeholder("API key").Password()
						if key.Submitted() {
							a.saveTTSKey(a.requests.ttsText)
						}
						if m3.Button(c, m3.ButtonSpec{Label: "Save", Kind: m3.Filled, Size: m3.Medium56, Key: "tts-save"}).Clicked() {
							a.saveTTSKey(a.requests.ttsText)
						}
					})
				}
				if a.requests.ttsStatus != "" {
					m3.Text(c, m3.BodyMedium, a.requests.ttsStatus).TextColor(sc.OnSurfaceVariant).MaxLines(4)
				}
				if a.requests.ttsError != "" {
					m3.Text(c, m3.BodyMedium, a.requests.ttsError).TextColor(sc.Error).MaxLines(3)
				}
			})
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
