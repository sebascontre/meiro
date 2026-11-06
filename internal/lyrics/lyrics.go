// Package lyrics is the lyrics of a song as the views show them: lines with
// the moment each is sung and, when the source times them, the words in a line
// with theirs.
package lyrics

import (
	"sort"
	"strings"
	"time"
)

// Word is one word of a line, or one syllable of a song in a language that
// counts them, and the moment it is sung.
type Word struct {
	At   time.Duration
	Text string
}

// Line is one sung line and the moment it starts. Words is empty for a source
// that times only lines.
type Line struct {
	At    time.Duration
	Text  string
	Words []Word
}

// IndexAt is the line being sung at position: the last one that has started.
// It is -1 before the first line.
func IndexAt(lines []Line, position time.Duration) int {
	return sort.Search(len(lines), func(i int) bool { return lines[i].At > position }) - 1
}

// IndexWordAt is the word being sung at position: the last one that has
// started. It is -1 before the first word.
func IndexWordAt(words []Word, position time.Duration) int {
	return sort.Search(len(words), func(i int) bool { return words[i].At > position }) - 1
}

// Text is the lines as plain text, one line each.
func Text(lines []Line) string {
	text := make([]string, len(lines))
	for i, line := range lines {
		text[i] = line.Text
	}
	return strings.TrimSpace(strings.Join(text, "\n"))
}
