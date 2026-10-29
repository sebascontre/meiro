package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/elianiva/meiro/player"
)

// speakRadio prefers a neural voice from Microsoft Edge's online service and
// falls back to the system voice when it is unreachable.
func (a *app) speakRadio(ctx context.Context, text string) error {
	if err := speakOnline(ctx, text); err != nil {
		log.Printf("online radio voice unavailable, using the system voice: %v", err)
		return speakRadioLine(ctx, text)
	}
	return nil
}

func speakOnline(ctx context.Context, text string) error {
	fetch, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	audio, err := edgeSpeech(fetch, text, edgeVoice)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp("", "meiro-radio-*.mp3")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, err = file.Write(audio)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	voice := player.New(playerOptions()...)
	defer voice.Stop()
	if err := voice.PlayContext(ctx, file.Name()); err != nil {
		return err
	}
	// Give ffmpeg a moment to start before treating silence as the end.
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

var spanishVoice = regexp.MustCompile(`(?m)^(.+?)\s+es[_-][A-Z0-9_]+\s+#`)

// speakRadioLine uses a Spanish voice already installed on the system. The
// caller runs it off the UI thread and only while a song has ended.
func speakRadioLine(ctx context.Context, text string) error {
	switch runtime.GOOS {
	case "darwin":
		return speakMac(ctx, text)
	case "windows":
		return speakWindows(ctx, text)
	case "linux":
		return speakLinux(ctx, text)
	default:
		return fmt.Errorf("radio voice is not supported on %s", runtime.GOOS)
	}
}

func speakMac(ctx context.Context, text string) error {
	voices, err := exec.CommandContext(ctx, "say", "-v", "?").Output()
	if err != nil {
		return fmt.Errorf("listing installed Mac voices: %w", err)
	}
	match := spanishVoice.FindSubmatch(voices)
	if len(match) < 2 {
		return errors.New("install a Spanish voice in macOS System Settings > Accessibility > Spoken Content")
	}
	voice := strings.TrimSpace(string(match[1]))
	if err := exec.CommandContext(ctx, "say", "-v", voice, "-r", "170", text).Run(); err != nil {
		return fmt.Errorf("speaking with the Spanish Mac voice: %w", err)
	}
	return nil
}

func speakWindows(ctx context.Context, text string) error {
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		powershell, err = exec.LookPath("pwsh.exe")
	}
	if err != nil {
		return errors.New("PowerShell is required for the Windows radio voice")
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(text))
	script := "$ErrorActionPreference='Stop'; Add-Type -AssemblyName System.Speech; " +
		"$s=New-Object System.Speech.Synthesis.SpeechSynthesizer; " +
		"$v=$s.GetInstalledVoices() | Where-Object {$_.VoiceInfo.Culture.Name -like 'es-*'} | Select-Object -First 1; " +
		"if ($null -eq $v) {throw 'Install a Spanish speech voice in Windows Settings.'}; " +
		"$s.SelectVoice($v.VoiceInfo.Name); $s.Rate=0; " +
		"$s.Speak([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('" + encoded + "'))); $s.Dispose()"
	encodedScript := utf16.Encode([]rune(script))
	utf16Bytes := make([]byte, len(encodedScript)*2)
	for i, unit := range encodedScript {
		binary.LittleEndian.PutUint16(utf16Bytes[i*2:], unit)
	}
	command := exec.CommandContext(ctx, powershell, "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-EncodedCommand", base64.StdEncoding.EncodeToString(utf16Bytes))
	if output, err := command.CombinedOutput(); err != nil {
		message := strings.TrimSpace(string(output))
		if message != "" {
			return fmt.Errorf("speaking with the Windows Spanish voice: %s", message)
		}
		return fmt.Errorf("speaking with the Windows Spanish voice: %w", err)
	}
	return nil
}

func speakLinux(ctx context.Context, text string) error {
	if binary, err := exec.LookPath("espeak-ng"); err == nil {
		return runVoiceCommand(ctx, binary, text, "-v", "es-la", "-s", "155", "--stdin")
	}
	if binary, err := exec.LookPath("espeak"); err == nil {
		return runVoiceCommand(ctx, binary, text, "-v", "es-la", "-s", "155", "--stdin")
	}
	if binary, err := exec.LookPath("spd-say"); err == nil {
		return runVoiceCommand(ctx, binary, "", "-l", "es", "-r", "-15", text)
	}
	return errors.New("install espeak-ng or Speech Dispatcher to use the Spanish radio voice")
}

func runVoiceCommand(ctx context.Context, command, input string, args ...string) error {
	cmd := exec.CommandContext(ctx, command, args...)
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		message := strings.TrimSpace(string(output))
		if message != "" {
			return fmt.Errorf("radio voice: %s", message)
		}
		return fmt.Errorf("radio voice: %w", err)
	}
	return nil
}
