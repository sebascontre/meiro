package twitch

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
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
		_, _ = server.Write([]byte(":tmi 366 x #chan :End of /NAMES\r\nPING :tmi\r\n"))
		if line, _ := r.ReadString('\n'); line != "PONG :tmi\r\n" {
			t.Errorf("pong = %q", line)
		}
		_, _ = server.Write([]byte(":a!a@a PRIVMSG #chan :hello\r\n"))
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

func TestValidateToken(t *testing.T) {
	missingScope := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "OAuth secret-token" && got != "OAuth token" {
			t.Errorf("authorization = %q", got)
		}
		if missingScope {
			_, _ = w.Write([]byte(`{"login":"SomeUser","scopes":["chat:read"]}`))
		} else {
			_, _ = w.Write([]byte(`{"login":"SomeUser","scopes":["chat:read","chat:edit"]}`))
		}
	}))
	defer server.Close()
	login, err := validate(context.Background(), "oauth:secret-token", server.Client(), server.URL)
	if err != nil || login != "someuser" {
		t.Fatalf("Validate = %q, %v", login, err)
	}

	missingScope = true
	if _, err := validate(context.Background(), "token", server.Client(), server.URL); err == nil {
		t.Fatal("accepted a token without chat:edit")
	}
}

func TestAuthenticatedSessionCanReply(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	sendReady := make(chan func(string) error, 1)
	sessionDone := make(chan struct{})
	go func() {
		defer close(sessionDone)
		sessionWithAuth(context.Background(), func(context.Context) (net.Conn, error) {
			return client, nil
		}, "channel", "someuser", "access-token", nil, nil, func(send func(string) error) {
			if send != nil {
				sendReady <- send
			}
		})
	}()

	reader := bufio.NewReader(server)
	var commands []string
	for range 4 {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		commands = append(commands, strings.TrimSpace(line))
	}
	want := []string{"CAP REQ :twitch.tv/tags", "PASS oauth:access-token", "NICK someuser", "JOIN #channel"}
	for i := range want {
		if commands[i] != want[i] {
			t.Fatalf("command %d = %q, want %q", i, commands[i], want[i])
		}
	}
	_, _ = server.Write([]byte(":tmi 366 someuser #channel :End of /NAMES\r\n"))
	var send func(string) error
	select {
	case send = <-sendReady:
	case <-time.After(time.Second):
		t.Fatal("authenticated connection did not expose a sender")
	}
	replyLine := make(chan string, 1)
	go func() {
		line, _ := reader.ReadString('\n')
		replyLine <- line
	}()
	if err := send("@viewer: Song was added at position 2."); err != nil {
		t.Fatal(err)
	}
	reply := <-replyLine
	if got, want := strings.TrimSpace(reply), "PRIVMSG #channel :@viewer: Song was added at position 2."; got != want {
		t.Fatalf("reply = %q, want %q", got, want)
	}
	if err := send("malicious\r\nPRIVMSG #other :hi"); err == nil {
		t.Fatal("accepted multiple IRC lines")
	}
	_ = server.Close()
	select {
	case <-sessionDone:
	case <-time.After(time.Second):
		t.Fatal("session did not stop after the connection closed")
	}
	if err := send("after close"); err == nil {
		t.Fatal("send after the connection closed unexpectedly succeeded")
	}
}
