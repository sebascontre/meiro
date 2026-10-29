package main

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestEdgeSecGEC(t *testing.T) {
	a := edgeSecGEC(time.Unix(1699999900, 0))
	b := edgeSecGEC(time.Unix(1700000000, 0))
	if len(a) != 64 || a != strings.ToUpper(a) || a != b {
		t.Fatalf("token should be uppercase sha256 stable within five minutes: %q %q", a, b)
	}
}

func TestEdgeFrames(t *testing.T) {
	var buf bytes.Buffer
	if err := writeFrame(&buf, strings.Repeat("é", 100)); err != nil {
		t.Fatal(err)
	}
	if buf.Bytes()[1]&0x80 == 0 {
		t.Fatal("client frames must be masked")
	}
	server := bufio.NewReader(bytes.NewReader([]byte{0x01, 0x02, 'a', 'b', 0x80, 0x01, 'c'}))
	op, msg, err := readFrame(server)
	if err != nil || op != 1 || string(msg) != "abc" {
		t.Fatalf("fragments: %d %q %v", op, msg, err)
	}
}

func TestEdgeSSMLEscapes(t *testing.T) {
	if got := edgeSSML("Tom & <Jerry>", edgeVoice); !strings.Contains(got, "Tom &amp; &lt;Jerry&gt;") {
		t.Fatal(got)
	}
}
