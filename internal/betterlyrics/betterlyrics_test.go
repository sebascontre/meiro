package betterlyrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/elianiva/meiro/internal/lyrics"
)

func TestParseTTML(t *testing.T) {
	document := `<?xml version="1.0" encoding="UTF-8"?>
<tt xmlns="http://www.w3.org/ns/ttml" xmlns:ttm="http://www.w3.org/ns/ttml#metadata" xml:lang="en">
<head><metadata><ttm:agent type="person" xml:id="v1"/></metadata></head>
<body dur="1:00.000"><div begin="0.000" end="30.000">
<p begin="0.000" end="2.000" ttm:agent="v1"><span begin="0.000" end="0.500">I</span> <span begin="0.500" end="2.000">lo—</span></p>
<p begin="2.500" end="4.000"><span begin="2.500" end="3.000">On</span> <span begin="3.000" end="3.500">the</span> <span ttm:role="x-bg"><span begin="3.500" end="4.000">way</span></span></p>
<p begin="00:01:30.500" end="00:01:32.000"><span begin="00:01:30.500" end="00:01:31.000">Last</span> <span begin="00:01:31.000" end="00:01:32.000">one</span></p>
</div></body></tt>`
	want := []lyrics.Line{
		{At: 0, Text: "I lo—", Words: []lyrics.Word{{At: 0, Text: "I "}, {At: 500 * time.Millisecond, Text: "lo—"}}},
		{At: 2500 * time.Millisecond, Text: "On the way", Words: []lyrics.Word{
			{At: 2500 * time.Millisecond, Text: "On "},
			{At: 3 * time.Second, Text: "the "},
			{At: 3500 * time.Millisecond, Text: "way"},
		}},
		{At: 90500 * time.Millisecond, Text: "Last one", Words: []lyrics.Word{
			{At: 90500 * time.Millisecond, Text: "Last "},
			{At: 91 * time.Second, Text: "one"},
		}},
	}
	if got := ParseTTML(document); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestParseTTMLKeepsTheTextOfALineWithoutWords(t *testing.T) {
	document := `<tt><body><div><p begin="1:00.000">Sing along</p><p begin="2:00.000"></p></div></body></tt>`
	want := []lyrics.Line{{At: time.Minute, Text: "Sing along"}}
	if got := ParseTTML(document); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestParseTTMLReadsSyllablesWithNoSpaceBetween(t *testing.T) {
	document := `<tt><body><div><p begin="0.000"><span begin="0.000">無</span><span begin="0.250">敵</span><span begin="0.500">の</span><span begin="0.750">笑顔</span></p></div></body></tt>`
	got := ParseTTML(document)
	if len(got) != 1 || got[0].Text != "無敵の笑顔" || len(got[0].Words) != 4 {
		t.Fatalf("got %#v", got)
	}
}

func TestParseTTMLToleratesSomethingElse(t *testing.T) {
	for _, document := range []string{"", "not xml", `<tt><body><div><p begin="bad">x</p></div></body></tt>`} {
		if got := ParseTTML(document); len(got) != 0 {
			t.Fatalf("%q parsed as %#v", document, got)
		}
	}
}

func TestGetAsksForTheTrackAndReadsTheTTML(t *testing.T) {
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path + "?" + r.URL.RawQuery
		if r.Header.Get("User-Agent") == "" {
			t.Errorf("no user agent")
		}
		_, _ = w.Write([]byte(`{"ttml":"<tt><body><div><p begin=\"1.000\"><span begin=\"1.000\">Hi</span></p></div></body></tt>"}`))
	}))
	defer srv.Close()
	got, err := (&Client{BaseURL: srv.URL}).Get(context.Background(), Query{Title: "E85", Artist: "Don Toliver", Album: "OCTANE", Duration: 154_400 * time.Millisecond})
	if err != nil || len(got) != 1 || got[0].Text != "Hi" {
		t.Fatalf("got %#v, %v", got, err)
	}
	want := "/getLyrics?a=Don+Toliver&al=OCTANE&d=154&s=E85"
	if asked != want {
		t.Fatalf("asked %q, want %q", asked, want)
	}
}

func TestGetReportsNoLyricsAsNothingFound(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		got, err := (&Client{BaseURL: srv.URL}).Get(context.Background(), Query{Title: "T", Artist: "A"})
		srv.Close()
		if err != nil || got != nil {
			t.Fatalf("%d gave %#v, %v", status, got, err)
		}
	}
}

func TestGetReportsAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if got, err := (&Client{BaseURL: srv.URL}).Get(context.Background(), Query{Title: "T"}); err == nil || got != nil {
		t.Fatalf("got %#v, %v", got, err)
	}
}
