package twitch

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestChannel(t *testing.T) {
	for in, want := range map[string]string{
		"Elianiva":                     "elianiva",
		"#some_one":                    "some_one",
		"https://www.twitch.tv/Foo123": "foo123",
		"twitch.tv/foo123/videos":      "foo123",
		"ab":                           "",
		"bad name":                     "",
		"":                             "",
	} {
		if got := Channel(in); got != want {
			t.Errorf("Channel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParse(t *testing.T) {
	m, ok := Parse(`@badges=;display-name=Some\sOne;id=1 :someone!someone@someone.tmi.twitch.tv PRIVMSG #chan :!sr daft punk`)
	if !ok || m.User != "Some One" || m.Text != "!sr daft punk" {
		t.Fatalf("got %+v, %v", m, ok)
	}
	m, ok = Parse(":nick!nick@nick.tmi.twitch.tv PRIVMSG #chan :hi")
	if !ok || m.User != "nick" || m.Text != "hi" {
		t.Fatalf("got %+v, %v", m, ok)
	}
	for _, line := range []string{"PING :tmi.twitch.tv", ":tmi.twitch.tv 001 x :Welcome", "@a=b :x!x@x JOIN #c", ""} {
		if _, ok := Parse(line); ok {
			t.Errorf("Parse(%q) accepted", line)
		}
	}
}

func TestRunJoinsAndReads(t *testing.T) {
	client, server := net.Pipe()
	var dials int
	open := func(context.Context) (net.Conn, error) {
		dials++
		if dials > 1 {
			return nil, context.Canceled
		}
		return client, nil
	}
	go func() {
		defer server.Close()
		r := bufio.NewReader(server)
		var sent []string
		for range 4 {
			line, _ := r.ReadString('\n')
			sent = append(sent, strings.TrimSpace(line))
		}
		if sent[3] != "JOIN #chan" {
			t.Errorf("sent %q", sent)
		}
		server.Write([]byte(":tmi 366 x #chan :End of /NAMES\r\nPING :tmi\r\n"))
		if line, _ := r.ReadString('\n'); line != "PONG :tmi\r\n" {
			t.Errorf("pong = %q", line)
		}
		server.Write([]byte(":a!a@a PRIVMSG #chan :hello\r\n"))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got := make(chan Message, 1)
	var states []bool
	go func() {
		m := <-got
		if m.User != "a" || m.Text != "hello" {
			t.Errorf("got %+v", m)
		}
		cancel()
	}()
	run(ctx, open, "chan", func(m Message) { got <- m }, func(c bool) { states = append(states, c) }, time.Millisecond, time.Millisecond)
	if len(states) == 0 || !states[0] {
		t.Errorf("states = %v", states)
	}
}
