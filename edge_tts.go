package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"strings"
	"time"
)

const (
	edgeHost        = "speech.platform.bing.com"
	edgeToken       = "6A5AA1D4EAFF4E9FB37E23D68491D6F4"
	edgeChromeMajor = "130"
	edgeVersion     = "1-130.0.2849.68"
	edgeVoice       = "es-MX-JorgeNeural"
	edgeFormat      = "audio-24khz-48kbitrate-mono-mp3"
	maxEdgeAudio    = 4 << 20
)

// edgeSecGEC is the anti-abuse token Microsoft's read-aloud service expects:
// a hash of the current time rounded to five minutes.
func edgeSecGEC(now time.Time) string {
	ticks := (now.Unix() + 11644473600) / 300 * 300 * 10000000
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d%s", ticks, edgeToken)))
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

func edgeSSML(text, voice string) string {
	return "<speak version='1.0' xmlns='http://www.w3.org/2001/10/synthesis' xml:lang='es-MX'>" +
		"<voice name='" + voice + "'><prosody pitch='-2Hz' rate='+4%' volume='+0%'>" +
		html.EscapeString(text) + "</prosody></voice></speak>"
}

func edgeStamp() string {
	return time.Now().UTC().Format("Mon Jan 02 2006 15:04:05") + " GMT+0000 (Coordinated Universal Time)"
}

func randomHex() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// edgeSpeech synthesizes text with a Microsoft Edge neural voice and returns
// MP3 audio. The service is unofficial, so callers must be ready for failure.
func edgeSpeech(ctx context.Context, text, voice string) ([]byte, error) {
	dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: &tls.Config{ServerName: edgeHost}}
	conn, err := dialer.DialContext(ctx, "tcp", edgeHost+":443")
	if err != nil {
		return nil, fmt.Errorf("connecting to the Edge voice service: %w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	key := make([]byte, 16)
	_, _ = rand.Read(key)
	path := "/consumer/speech/synthesize/readaloud/edge/v1?TrustedClientToken=" + edgeToken +
		"&ConnectionId=" + randomHex() + "&Sec-MS-GEC=" + edgeSecGEC(time.Now()) + "&Sec-MS-GEC-Version=" + edgeVersion
	request := "GET " + path + " HTTP/1.1\r\nHost: " + edgeHost + "\r\n" +
		"Connection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: " + base64.StdEncoding.EncodeToString(key) + "\r\n" +
		"Origin: chrome-extension://jdiccldimpdaibmpdkjnbmckianbfold\r\n" +
		"Pragma: no-cache\r\nCache-Control: no-cache\r\n" +
		"User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + edgeChromeMajor + ".0.0.0 Safari/537.36 Edg/" + edgeChromeMajor + ".0.0.0\r\n" +
		"Accept-Encoding: identity\r\nAccept-Language: en-US,en;q=0.9\r\n\r\n"
	if _, err := io.WriteString(conn, request); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if !strings.Contains(status, " 101 ") {
		return nil, fmt.Errorf("the Edge voice service refused the connection: %s", strings.TrimSpace(status))
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if line == "\r\n" {
			break
		}
	}

	config := "X-Timestamp:" + edgeStamp() + "\r\nContent-Type:application/json; charset=utf-8\r\nPath:speech.config\r\n\r\n" +
		`{"context":{"synthesis":{"audio":{"metadataoptions":{"sentenceBoundaryEnabled":"false","wordBoundaryEnabled":"false"},"outputFormat":"` + edgeFormat + `"}}}}`
	if err := writeFrame(conn, config); err != nil {
		return nil, err
	}
	ssml := "X-RequestId:" + randomHex() + "\r\nContent-Type:application/ssml+xml\r\nX-Timestamp:" + edgeStamp() + "Z\r\nPath:ssml\r\n\r\n" + edgeSSML(text, voice)
	if err := writeFrame(conn, ssml); err != nil {
		return nil, err
	}

	var audio []byte
	for {
		opcode, payload, err := readFrame(reader)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("reading Edge voice audio: %w", err)
		}
		switch opcode {
		case 1:
			if strings.Contains(string(payload), "Path:turn.end") {
				if len(audio) == 0 {
					return nil, errors.New("the Edge voice service returned no audio")
				}
				return audio, nil
			}
		case 2:
			if len(payload) < 2 {
				continue
			}
			n := int(binary.BigEndian.Uint16(payload))
			if len(payload) < 2+n {
				return nil, errors.New("malformed Edge voice frame")
			}
			if strings.Contains(string(payload[2:2+n]), "Path:audio") {
				audio = append(audio, payload[2+n:]...)
				if len(audio) > maxEdgeAudio {
					return nil, errors.New("the Edge voice audio is too large")
				}
			}
		case 8:
			return nil, errors.New("the Edge voice service closed the connection")
		}
	}
}

// writeFrame sends a masked text frame, as clients must.
func writeFrame(w io.Writer, text string) error {
	payload := []byte(text)
	header := []byte{0x81}
	switch n := len(payload); {
	case n < 126:
		header = append(header, 0x80|byte(n))
	case n < 1<<16:
		header = append(header, 0x80|126, byte(n>>8), byte(n))
	default:
		header = append(header, 0x80|127, 0, 0, 0, 0, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	mask := make([]byte, 4)
	_, _ = rand.Read(mask)
	header = append(header, mask...)
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	_, err := w.Write(append(header, payload...))
	return err
}

// readFrame returns one complete message, joining fragments.
func readFrame(r *bufio.Reader) (byte, []byte, error) {
	var message []byte
	var first byte
	for {
		head := make([]byte, 2)
		if _, err := io.ReadFull(r, head); err != nil {
			return 0, nil, err
		}
		fin, opcode := head[0]&0x80 != 0, head[0]&0x0f
		length := uint64(head[1] & 0x7f)
		switch length {
		case 126:
			ext := make([]byte, 2)
			if _, err := io.ReadFull(r, ext); err != nil {
				return 0, nil, err
			}
			length = uint64(binary.BigEndian.Uint16(ext))
		case 127:
			ext := make([]byte, 8)
			if _, err := io.ReadFull(r, ext); err != nil {
				return 0, nil, err
			}
			length = binary.BigEndian.Uint64(ext)
		}
		if length > maxEdgeAudio {
			return 0, nil, errors.New("websocket frame too large")
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(r, payload); err != nil {
			return 0, nil, err
		}
		if opcode == 9 || opcode == 10 {
			continue
		}
		if opcode != 0 {
			first = opcode
		}
		message = append(message, payload...)
		if fin {
			return first, message, nil
		}
	}
}
