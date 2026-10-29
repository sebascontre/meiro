// Package twitch reads the chat of a Twitch channel. It connects without an
// account, which Twitch allows for reading, so it can only listen.
package twitch

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"math/rand/v2"
	"net"
	"strings"
	"time"
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
	conn, err := open(ctx)
	if err != nil {
		return false
	}
	defer conn.Close()
	defer context.AfterFunc(ctx, func() { conn.Close() })()

	nick := fmt.Sprintf("justinfan%d", 10000+rand.IntN(80000))
	for _, line := range []string{
		"CAP REQ :twitch.tv/tags",
		"PASS SCHMOOPIIE",
		"NICK " + nick,
		"JOIN #" + channel,
	} {
		if _, err := fmt.Fprintf(conn, "%s\r\n", line); err != nil {
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
			if _, err := fmt.Fprintf(conn, "PONG%s\r\n", strings.TrimPrefix(line, "PING")); err != nil {
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
			}
		default:
			if m, ok := Parse(line); ok {
				onMessage(m)
			}
		}
	}
}
