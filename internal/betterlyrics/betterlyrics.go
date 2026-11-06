// Package betterlyrics fetches song lyrics from https://api.betterlyrics.org,
// the service behind the Better Lyrics extension, which times each word of a
// line. It reads the TTML it answers with into lines with their words.
package betterlyrics

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/elianiva/meiro/internal/lyrics"
)

// userAgent names the app, as the service asks of its clients.
const userAgent = "Meiro (https://github.com/elianiva/meiro)"

// maxBody bounds the answer read; lyrics are a few kilobytes.
const maxBody = 1 << 20

// Query names the track to look for. Album and Duration improve the match, and
// the service keys its cache on them: a request without them often misses the
// entry a fuller request finds.
type Query struct {
	Title, Artist, Album string
	Duration             time.Duration
}

// Client asks Better Lyrics. The zero value uses the real service.
type Client struct {
	HTTP *http.Client
	// BaseURL replaces https://api.betterlyrics.org, for tests.
	BaseURL string
}

type response struct {
	TTML string `json:"ttml"`
}

// Get finds the lyrics of a track, with the moment each word is sung. It
// returns no lines and no error when the service has them not: a song it has
// not cached needs an API key, which the service does not issue.
func (c *Client) Get(ctx context.Context, q Query) ([]lyrics.Line, error) {
	if q.Title == "" && q.Artist == "" {
		return nil, nil
	}
	values := url.Values{"s": {q.Title}, "a": {q.Artist}}
	if q.Album != "" {
		values.Set("al", q.Album)
	}
	if q.Duration > 0 {
		values.Set("d", strconv.Itoa(int(q.Duration.Round(time.Second)/time.Second)))
	}
	base := c.BaseURL
	if base == "" {
		base = "https://api.betterlyrics.org"
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/getLyrics?"+values.Encode(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", userAgent)
	got, err := client.Do(request)
	if err != nil {
		log.Printf("lyrics: betterlyrics GET %s failed: %v", request.URL, err)
		return nil, err
	}
	defer got.Body.Close()
	log.Printf("lyrics: betterlyrics GET %s -> %s", request.URL, got.Status)
	switch got.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusUnauthorized, http.StatusUnprocessableEntity,
		http.StatusTooManyRequests, http.StatusServiceUnavailable:
		// No lyrics this time: the service has none, has not cached them,
		// is rate limiting, or is serving cached ones only. The next source
		// is tried.
		return nil, nil
	default:
		return nil, fmt.Errorf("betterlyrics: %s", got.Status)
	}
	body, err := io.ReadAll(io.LimitReader(got.Body, maxBody))
	if err != nil {
		return nil, err
	}
	var answer response
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil, errors.New("betterlyrics: unreadable answer")
	}
	lines := ParseTTML(answer.TTML)
	if len(lines) == 0 {
		return nil, nil
	}
	return lines, nil
}

// ParseTTML reads the lines of a TTML document, with the moment each word
// starts: each <p> is a line and each <span> in it that starts at a time is
// one of its words, or syllables. The text between the spans is the spacing
// between them, which the document depends on, so it is kept with the word
// before it. Spans that only group others, such as background vocals, are
// read through, and a line with no timed span keeps the text it has.
func ParseTTML(document string) []lyrics.Line {
	if strings.TrimSpace(document) == "" {
		return nil
	}
	decoder := xml.NewDecoder(strings.NewReader(document))
	var lines []lyrics.Line
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "p" {
			continue
		}
		line, ok, err := readLine(decoder, start)
		if err != nil {
			break
		}
		if ok {
			lines = append(lines, line)
		}
	}
	return lines
}

// readLine reads a <p>, up to and including its end, into a line.
func readLine(decoder *xml.Decoder, start xml.StartElement) (lyrics.Line, bool, error) {
	at, timed := clock(start, "begin")
	reader := &spanReader{decoder: decoder}
	if err := reader.walk(start.Name.Local); err != nil {
		return lyrics.Line{}, false, err
	}
	line := lyrics.Line{At: at, Text: strings.TrimSpace(reader.plain.String()), Words: reader.words}
	if len(line.Words) > 0 && !timed {
		line.At = line.Words[0].At
	}
	// A line with no time of its own and no word that has one cannot be
	// followed, and only adds noise to a list of them.
	return line, line.Text != "" && (timed || len(line.Words) > 0), nil
}

// spanReader collects the words and the text of a line.
type spanReader struct {
	decoder *xml.Decoder
	// words are the timed spans met so far, with the text between them
	// appended to the one before it.
	words []lyrics.Word
	// plain is every piece of text of the line, in order, whether it is
	// timed or not.
	plain strings.Builder
	// lead is text before the first word, which would otherwise be lost.
	lead string
}

// walk reads a line's text and words up to its end.
func (r *spanReader) walk(end string) error {
	for {
		token, err := r.decoder.Token()
		if err != nil {
			return err
		}
		switch t := token.(type) {
		case xml.CharData:
			r.text(string(t))
		case xml.StartElement:
			if t.Name.Local != "span" {
				// A line holds nothing else that matters: skip the subtree.
				if err := r.decoder.Skip(); err != nil {
					return err
				}
				continue
			}
			if at, timed := clock(t, "begin"); timed {
				text, err := r.spanText(t.Name.Local)
				if err != nil {
					return err
				}
				r.plain.WriteString(text)
				r.words = append(r.words, lyrics.Word{At: at, Text: r.lead + text})
				r.lead = ""
				continue
			}
			// A span that groups others, such as background vocals, is
			// read through as if its words were the line's own.
			if err := r.walk(t.Name.Local); err != nil {
				return err
			}
		case xml.EndElement:
			if t.Name.Local == end {
				return nil
			}
		}
	}
}

// text keeps text between spans with the word before it, which is how the
// document spaces its words.
func (r *spanReader) text(s string) {
	r.plain.WriteString(s)
	switch {
	case len(r.words) > 0:
		last := &r.words[len(r.words)-1]
		last.Text += s
	case strings.TrimSpace(s) != "":
		r.lead += s
	}
}

// spanText reads the text of a span, nested elements included, up to its end.
func (r *spanReader) spanText(end string) (string, error) {
	var text strings.Builder
	for {
		token, err := r.decoder.Token()
		if err != nil {
			return "", err
		}
		switch t := token.(type) {
		case xml.CharData:
			text.WriteString(string(t))
		case xml.StartElement:
			inner, err := r.spanText(t.Name.Local)
			if err != nil {
				return "", err
			}
			text.WriteString(inner)
		case xml.EndElement:
			if t.Name.Local == end {
				return text.String(), nil
			}
		}
	}
}

// clock reads a time attribute of an element: "hh:mm:ss.mmm", "mm:ss.mmm" or
// seconds.
func clock(element xml.StartElement, name string) (time.Duration, bool) {
	for _, attr := range element.Attr {
		if attr.Name.Local == name {
			return parseClock(attr.Value)
		}
	}
	return 0, false
}

// parseClock reads a clock time, as TTML writes one.
func parseClock(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	parts := strings.Split(value, ":")
	if len(parts) > 3 {
		return 0, false
	}
	var total time.Duration
	for i, part := range parts {
		amount, err := strconv.ParseFloat(part, 64)
		if err != nil || amount < 0 {
			return 0, false
		}
		switch len(parts) - 1 - i {
		case 0:
			total += time.Duration(amount * float64(time.Second))
		case 1:
			total += time.Duration(amount * float64(time.Minute))
		default:
			total += time.Duration(amount * float64(time.Hour))
		}
	}
	return total, true
}
