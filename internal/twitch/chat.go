// Package twitch reads the chat of a Twitch channel. It connects without an
// account, which Twitch allows for reading, so it can only listen.
package twitch

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const address = "irc.chat.twitch.tv:6697"

// Message is one chat message.
type Message struct {
	// User is the name the chatter shows.
	User string
	Text string
}

// Channel normalises what the user typed for a channel, which may be a name,
// "#name" or a link to the channel, into the lower-case login. It returns ""
// when that is not a valid login.
func Channel(input string) string {
	name := strings.TrimSpace(input)
	if i := strings.Index(name, "twitch.tv/"); i >= 0 {
		name = name[i+len("twitch.tv/"):]
	}
	name, _, _ = strings.Cut(name, "?")
	name, _, _ = strings.Cut(name, "/")
	name = strings.ToLower(strings.TrimPrefix(name, "#"))
	if len(name) < 3 || len(name) > 25 {
		return ""
	}
	for _, r := range name {
		if r != '_' && (r < '0' || r > '9') && (r < 'a' || r > 'z') {
			return ""
		}
	}
	return name
}

// Parse reads a line of the chat protocol, and reports whether it is a chat
// message.
func Parse(line string) (Message, bool) {
	tags := ""
	if strings.HasPrefix(line, "@") {
		var ok bool
		tags, line, ok = strings.Cut(line[1:], " ")
		if !ok {
			return Message{}, false
		}
	}
	if !strings.HasPrefix(line, ":") {
		return Message{}, false
	}
	prefix, rest, ok := strings.Cut(line[1:], " ")
	if !ok {
		return Message{}, false
	}
	command, rest, _ := strings.Cut(rest, " ")
	if command != "PRIVMSG" {
		return Message{}, false
	}
	_, text, ok := strings.Cut(rest, " :")
	if !ok {
		return Message{}, false
	}
	user, _, _ := strings.Cut(prefix, "!")
	for _, tag := range strings.Split(tags, ";") {
		if name, ok := strings.CutPrefix(tag, "display-name="); ok && name != "" {
			user = unescapeTag(name)
		}
	}
	if user == "" {
		return Message{}, false
	}
	return Message{User: user, Text: strings.TrimRight(text, "\r\n")}, true
}

func unescapeTag(value string) string {
	return strings.NewReplacer(`\s`, " ", `\:`, ";", `\\`, `\`, `\r`, "", `\n`, "").Replace(value)
}

// Dialer opens the connection to the chat server.
type Dialer func(ctx context.Context) (net.Conn, error)

func dial(ctx context.Context) (net.Conn, error) {
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 15 * time.Second}}
	return d.DialContext(ctx, "tcp", address)
}

// Run listens to the chat of channel and calls onMessage with each message,
// one at a time, until ctx ends. It connects again, after a wait that grows,
// when the connection is lost. onState is told true once the channel is
// joined and false when the connection is lost; it may be nil.
func Run(ctx context.Context, channel string, onMessage func(Message), onState func(connected bool)) {
	run(ctx, dial, channel, onMessage, onState, time.Second, time.Minute)
}

// Validate checks a Twitch access token and reports the account login. The
// token must allow reading and sending chat messages.
func Validate(ctx context.Context, token string) (string, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	return validate(ctx, token, client, "https://id.twitch.tv/oauth2/validate")
}

func validate(ctx context.Context, token string, client *http.Client, endpoint string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "OAuth "+strings.TrimPrefix(strings.TrimSpace(token), "oauth:"))
	response, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("validating Twitch token: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", errors.New("Twitch rejected the token; check that it is valid")
	}
	var result struct {
		Login  string   `json:"login"`
		Scopes []string `json:"scopes"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result); err != nil {
		return "", fmt.Errorf("reading Twitch token details: %w", err)
	}
	scopes := make(map[string]bool, len(result.Scopes))
	for _, scope := range result.Scopes {
		scopes[scope] = true
	}
	if !scopes["chat:read"] || !scopes["chat:edit"] {
		return "", errors.New("Twitch token needs the chat:read and chat:edit permissions")
	}
	if Channel(result.Login) == "" {
		return "", errors.New("Twitch did not identify the account for this token")
	}
	return strings.ToLower(result.Login), nil
}

// RunAuthenticated reads a channel's chat and provides a sender after joining.
// The token must have chat:read and chat:edit permissions.
func RunAuthenticated(ctx context.Context, channel, login, token string, onMessage func(Message), onState func(bool), onSend func(func(string) error)) {
	wait := time.Second
	for ctx.Err() == nil {
		joined := sessionWithAuth(ctx, dial, channel, login, token, onMessage, onState, onSend)
		if onSend != nil {
			onSend(nil)
		}
		if joined {
			if onState != nil {
				onState(false)
			}
			wait = time.Second
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		wait = min(wait*2, time.Minute)
	}
}

func run(ctx context.Context, open Dialer, channel string, onMessage func(Message), onState func(bool), minWait, maxWait time.Duration) {
	wait := minWait
	for ctx.Err() == nil {
		joined := session(ctx, open, channel, onMessage, onState)
		if joined {
			if onState != nil {
				onState(false)
			}
			wait = minWait
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		wait = min(wait*2, maxWait)
	}
}

// session is one connection, which ends when it fails or ctx ends. It reports
// whether the channel was joined.
func session(ctx context.Context, open Dialer, channel string, onMessage func(Message), onState func(bool)) (joined bool) {
	return sessionWithAuth(ctx, open, channel, "", "", onMessage, onState, nil)
}

func sessionWithAuth(ctx context.Context, open Dialer, channel, login, token string, onMessage func(Message), onState func(bool), onSend func(func(string) error)) (joined bool) {
	conn, err := open(ctx)
	if err != nil {
		return false
	}
	defer conn.Close()
	defer context.AfterFunc(ctx, func() { conn.Close() })()

	nick, password := login, "SCHMOOPIIE"
	if token != "" {
		password = "oauth:" + strings.TrimPrefix(strings.TrimSpace(token), "oauth:")
	} else {
		nick = fmt.Sprintf("justinfan%d", 10000+rand.IntN(80000))
	}
	var writeMu sync.Mutex
	var chatMu sync.Mutex
	var nextChatMessage time.Time
	write := func(line string) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return err
		}
		_, err := fmt.Fprintf(conn, "%s\r\n", line)
		return err
	}
	for _, line := range []string{"CAP REQ :twitch.tv/tags", "PASS " + password, "NICK " + nick, "JOIN #" + channel} {
		if err := write(line); err != nil {
			return false
		}
	}
	reader := bufio.NewReaderSize(conn, 16<<10)
	for {
		// Twitch pings every few minutes; silence for longer is a dead link.
		_ = conn.SetReadDeadline(time.Now().Add(6 * time.Minute))
		line, err := reader.ReadString('\n')
		if err != nil {
			return joined
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "PING"):
			if err := write("PONG" + strings.TrimPrefix(line, "PING")); err != nil {
				return joined
			}
		case strings.Contains(line, " RECONNECT"), strings.HasPrefix(line, "RECONNECT"):
			return joined
		case strings.Contains(line, " 366 "):
			if !joined {
				joined = true
				if onState != nil {
					onState(true)
				}
				if onSend != nil && token != "" {
					onSend(func(text string) error {
						if strings.ContainsAny(text, "\r\n") || strings.TrimSpace(text) == "" {
							return errors.New("Twitch chat message must be one non-empty line")
						}
						if utf8.RuneCountInString(text) > 500 {
							return errors.New("Twitch chat message is longer than 500 characters")
						}
						chatMu.Lock()
						defer chatMu.Unlock()
						if wait := time.Until(nextChatMessage); wait > 0 {
							timer := time.NewTimer(wait)
							defer timer.Stop()
							select {
							case <-ctx.Done():
								return ctx.Err()
							case <-timer.C:
							}
						}
						if err := ctx.Err(); err != nil {
							return err
						}
						nextChatMessage = time.Now().Add(1600 * time.Millisecond)
						return write("PRIVMSG #" + channel + " :" + text)
					})
				}
			}
		default:
			if m, ok := Parse(line); ok {
				onMessage(m)
			}
		}
	}
}
