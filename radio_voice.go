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
	"strings"
	"time"

	"github.com/elianiva/meiro/player"
)

var googleSpeechEndpoint = googleSpeechURL

const (
	googleSpeechURL   = "https://texttospeech.googleapis.com/v1/text:synthesize"
	googleSpeechVoice = "es-US-Chirp3-HD-Puck"
	googleSpeechLang  = "es-US"
	maxSpeechAudio    = 4 << 20
)

// googleSpeech synthesizes text with Google Cloud Text-to-Speech and returns
// MP3 audio.
func googleSpeech(ctx context.Context, endpoint, apiKey, text string) ([]byte, error) {
	body, err := json.Marshal(map[string]any{
		"input":       map[string]string{"text": text},
		"voice":       map[string]string{"languageCode": googleSpeechLang, "name": googleSpeechVoice},
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
func fetchRadioClip(ctx context.Context, apiKey, text string) (string, error) {
	fetch, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	audio, err := googleSpeech(fetch, googleSpeechEndpoint, apiKey, text)
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
		if clip, err = fetchRadioClip(ctx, a.requests.ttsKey, text); err != nil {
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
