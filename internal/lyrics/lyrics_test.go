package lyrics

import (
	"testing"
	"time"
)

func TestIndexAt(t *testing.T) {
	lines := []Line{{At: 5 * time.Second, Text: "a"}, {At: 10 * time.Second, Text: "b"}}
	for _, c := range []struct {
		at   time.Duration
		want int
	}{{0, -1}, {4999 * time.Millisecond, -1}, {5 * time.Second, 0}, {9999 * time.Millisecond, 0}, {10 * time.Second, 1}, {time.Hour, 1}} {
		if got := IndexAt(lines, c.at); got != c.want {
			t.Errorf("IndexAt(%v) = %d, want %d", c.at, got, c.want)
		}
	}
}

func TestIndexWordAt(t *testing.T) {
	words := []Word{{At: time.Second, Text: "one "}, {At: 2 * time.Second, Text: "two "}, {At: 2500 * time.Millisecond, Text: "three"}}
	for _, c := range []struct {
		at   time.Duration
		want int
	}{{0, -1}, {999 * time.Millisecond, -1}, {time.Second, 0}, {1999 * time.Millisecond, 0}, {2 * time.Second, 1}, {2500 * time.Millisecond, 2}, {time.Minute, 2}} {
		if got := IndexWordAt(words, c.at); got != c.want {
			t.Errorf("IndexWordAt(%v) = %d, want %d", c.at, got, c.want)
		}
	}
}

func TestTextJoinsLines(t *testing.T) {
	got := Text([]Line{{Text: " one "}, {Text: ""}, {Text: "two"}})
	if want := "one \n\ntwo"; got != want {
		t.Fatalf("Text() = %q, want %q", got, want)
	}
}
