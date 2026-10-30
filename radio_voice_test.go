package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elianiva/meiro/player"
	"github.com/elianiva/meiro/youtube"
)

func TestSpeakRadioPlaysTheSynthesizedClip(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	mp3 := filepath.Join(t.TempDir(), "tone.mp3")
	if out, err := exec.Command(ffmpeg, "-f", "lavfi", "-i", "sine=frequency=440:duration=1", mp3).CombinedOutput(); err != nil {
		t.Fatalf("making a clip: %v\n%s", err, out)
	}
	data, err := os.ReadFile(mp3)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"audioContent":"` + base64.StdEncoding.EncodeToString(data) + `"}`))
	}))
	defer server.Close()
	oldEndpoint, oldOptions := googleSpeechEndpoint, radioPlayerOptions
	defer func() { googleSpeechEndpoint, radioPlayerOptions = oldEndpoint, oldOptions }()
	googleSpeechEndpoint = server.URL
	radioPlayerOptions = func() []player.Option { return []player.Option{player.WithFFmpeg(ffmpeg), player.WithNullAudio()} }

	a := newTestApp(t)
	a.requests.ttsKey = "k"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	if err := a.speakRadio(ctx, "hola", ""); err != nil {
		t.Fatalf("speakRadio: %v", err)
	}
	t.Logf("finished in %s", time.Since(start))
}

func TestRadioTransitionSpeaksThenStartsTheNextSong(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	tone := filepath.Join(t.TempDir(), "tone.mp3")
	if out, err := exec.Command(ffmpeg, "-f", "lavfi", "-i", "sine=frequency=440:duration=1", tone).CombinedOutput(); err != nil {
		t.Fatalf("making a tone: %v\n%s", err, out)
	}
	a := newTestApp(t)
	a.settings.SongRequests, a.settings.RadioAlways = true, true
	a.requests.ttsKey = "k"
	spoken := ""
	a.requests.speakRadio = func(_ context.Context, line, _ string) error { spoken = line; return nil }
	first := youtube.MusicItem{VideoID: "aaaaaaaaaaa", Title: "Uno", Kind: youtube.KindTrack}
	second := youtube.MusicItem{VideoID: "bbbbbbbbbbb", Title: "Dos", Kind: youtube.KindTrack}
	a.queue.items, a.queue.index, a.current = []youtube.MusicItem{first, second}, 0, first
	if err := a.player.Play(tone); err != nil {
		t.Fatal(err)
	}
	for start := time.Now(); !a.player.Ended(); {
		if time.Since(start) > 10*time.Second {
			t.Fatal("the tone never ended")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !a.radioTransition(first, second, 1) {
		t.Fatal("no radio break at the end of the song")
	}
	if !strings.Contains(spoken, "Uno") || !strings.Contains(spoken, "Dos") {
		t.Fatalf("spoken line = %q", spoken)
	}
	if a.current.VideoID != "bbbbbbbbbbb" {
		t.Fatalf("the next song did not start after the break: current %q", a.current.VideoID)
	}
}

// toneThenSilence makes a clip that sounds for a second and is then silent.
func toneThenSilence(t *testing.T, ffmpeg string, silence int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tail.mp3")
	out, err := exec.Command(ffmpeg, "-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-af", fmt.Sprintf("apad=pad_dur=%d", silence), "-t", fmt.Sprint(1+silence), path).CombinedOutput()
	if err != nil {
		t.Fatalf("making a clip: %v\n%s", err, out)
	}
	return path
}

func TestSilentTailMovesOnBeforeTheSongEnds(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	clip := toneThenSilence(t, ffmpeg, 6)
	a := newTestApp(t)
	a.settings.SongRequests = true
	first := youtube.MusicItem{VideoID: "aaaaaaaaaaa", Title: "Uno", Kind: youtube.KindTrack}
	second := youtube.MusicItem{VideoID: "bbbbbbbbbbb", Title: "Dos", Kind: youtube.KindTrack}
	a.queue.items, a.queue.index, a.current = []youtube.MusicItem{first, second}, 0, first
	a.total = 7 * time.Second
	if err := a.player.Play(clip); err != nil {
		t.Fatal(err)
	}
	moved := false
	for start := time.Now(); time.Since(start) < 10*time.Second; time.Sleep(20 * time.Millisecond) {
		if a.skipSilentTail() {
			moved = true
			break
		}
	}
	if !moved || a.current.VideoID != "bbbbbbbbbbb" {
		t.Fatalf("moved=%v current=%q silence=%s", moved, a.current.VideoID, a.player.TrailingSilence())
	}
}

func TestSilentTailIsIgnoredOutsideSongRequests(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	clip := toneThenSilence(t, ffmpeg, 6)
	a := newTestApp(t)
	a.total = 7 * time.Second
	if err := a.player.Play(clip); err != nil {
		t.Fatal(err)
	}
	for start := time.Now(); a.player.TrailingSilence() < 2*time.Second && time.Since(start) < 10*time.Second; time.Sleep(20 * time.Millisecond) {
	}
	if a.skipSilentTail() {
		t.Fatal("skipped with Song requests off")
	}
}
