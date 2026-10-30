package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/elianiva/meiro/player"
)

var googleSpeechEndpoint = googleSpeechURL

const (
	googleSpeechURL   = "https://texttospeech.googleapis.com/v1/text:synthesize"
	googleVoicesURL   = "https://texttospeech.googleapis.com/v1/voices"
	googleSpeechVoice = "es-US-Chirp3-HD-Puck"
	maxSpeechAudio    = 4 << 20
)

// voiceLanguage is the language code a Google voice name starts with, such as
// "es-US" for "es-US-Chirp3-HD-Puck".
func voiceLanguage(voice string) string {
	parts := strings.SplitN(voice, "-", 3)
	if len(parts) < 3 {
		return "es-US"
	}
	return parts[0] + "-" + parts[1]
}

// chirpNames are the Chirp 3: HD voices, offered in Spanish when Google's own
// list cannot be fetched.
var chirpNames = []string{
	"Achernar", "Achird", "Algenib", "Algieba", "Alnilam", "Aoede", "Autonoe", "Callirrhoe", "Charon", "Despina",
	"Enceladus", "Erinome", "Fenrir", "Gacrux", "Iapetus", "Kore", "Laomedeia", "Leda", "Orus", "Puck",
	"Pulcherrima", "Rasalgethi", "Sadachbia", "Sadaltager", "Schedar", "Sulafat", "Umbriel", "Vindemiatrix",
	"Zephyr", "Zubenelgenubi",
}

func defaultRadioVoices() []string {
	voices := make([]string, len(chirpNames))
	for i, name := range chirpNames {
		voices[i] = "es-US-Chirp3-HD-" + name
	}
	return voices
}

// googleVoices lists the Spanish Chirp 3: HD voices Google offers, sorted.
func googleVoices(ctx context.Context, endpoint, apiKey string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?key="+url.QueryEscape(apiKey), nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, errors.New("could not reach Google Text-to-Speech")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Google Text-to-Speech voices: %s", resp.Status)
	}
	var reply struct {
		Voices []struct {
			Name string `json:"name"`
		} `json:"voices"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&reply); err != nil {
		return nil, fmt.Errorf("reading the Google voices: %w", err)
	}
	var voices []string
	for _, v := range reply.Voices {
		if strings.HasPrefix(v.Name, "es-") && strings.Contains(v.Name, "-Chirp3-HD-") {
			voices = append(voices, v.Name)
		}
	}
	if len(voices) == 0 {
		return nil, errors.New("Google lists no Spanish Chirp 3: HD voices")
	}
	slices.Sort(voices)
	return voices, nil
}

// voiceLabel is a voice as the menu shows it: "Puck (es-US)".
func voiceLabel(voice string) string {
	_, name, ok := strings.Cut(voice, "-Chirp3-HD-")
	if !ok {
		return voice
	}
	return name + " (" + voiceLanguage(voice) + ")"
}

// googleSpeech synthesizes text with Google Cloud Text-to-Speech and returns
// MP3 audio.
func googleSpeech(ctx context.Context, endpoint, apiKey, voice, text string) ([]byte, error) {
	if voice == "" {
		voice = googleSpeechVoice
	}
	body, err := json.Marshal(map[string]any{
		"input":       map[string]string{"text": text},
		"voice":       map[string]string{"languageCode": voiceLanguage(voice), "name": voice},
		"audioConfig": map[string]any{"audioEncoding": "MP3", "speakingRate": 1.0},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"?key="+url.QueryEscape(apiKey), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, errors.New("could not reach Google Text-to-Speech")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSpeechAudio*2))
	if err != nil {
		return nil, fmt.Errorf("reading Google Text-to-Speech: %w", err)
	}
	var reply struct {
		AudioContent string `json:"audioContent"`
		Error        struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &reply)
	if resp.StatusCode != http.StatusOK {
		message := reply.Error.Message
		if message == "" {
			message = resp.Status
		}
		return nil, fmt.Errorf("Google Text-to-Speech: %s", message)
	}
	audio, err := base64.StdEncoding.DecodeString(reply.AudioContent)
	if err != nil || len(audio) == 0 {
		return nil, errors.New("Google Text-to-Speech returned no audio")
	}
	return audio, nil
}

// fetchRadioClip synthesizes text into a temporary MP3 the caller removes.
func fetchRadioClip(ctx context.Context, apiKey, voice, text string) (string, error) {
	fetch, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	audio, err := googleSpeech(fetch, googleSpeechEndpoint, apiKey, voice, text)
	if err != nil {
		return "", err
	}
	file, err := os.CreateTemp("", "meiro-radio-*.mp3")
	if err != nil {
		return "", err
	}
	_, err = file.Write(audio)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(file.Name())
		return "", err
	}
	return file.Name(), nil
}

// speakRadio plays the announcement through Meiro's own audio output. clip is
// audio already fetched ahead of time, or "".
func (a *app) speakRadio(ctx context.Context, text, clip string) error {
	if clip == "" {
		var err error
		if clip, err = fetchRadioClip(ctx, a.requests.ttsKey, a.settings.RadioVoice, text); err != nil {
			return err
		}
	}
	defer os.Remove(clip)
	return playRadioClip(ctx, clip)
}

// radioPlayerOptions are the options of the player that speaks; tests replace
// it to run without a sound card.
var radioPlayerOptions = playerOptions

func playRadioClip(ctx context.Context, path string) error {
	voice := player.New(radioPlayerOptions()...)
	defer voice.Stop()
	if err := voice.PlayContext(ctx, path); err != nil {
		return err
	}
	for started := time.Now(); ; {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		if voice.Ended() {
			return nil
		}
		if time.Since(started) > 30*time.Second {
			return errors.New("the radio voice did not finish playing")
		}
	}
}

// cleanSpeechKey trims a pasted API key.
func cleanSpeechKey(text string) string { return strings.TrimSpace(text) }
