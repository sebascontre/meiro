package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elianiva/meiro/youtube"
)

func TestOBSWidgetStateUsesCurrentAndFollowingQueueItems(t *testing.T) {
	a := newTestApp(t)
	a.settings.OBSWidget = true
	a.current = youtube.MusicItem{VideoID: "current", Title: "Now", Artist: "Artist", RequestedBy: "ana"}
	a.queue.items = []youtube.MusicItem{
		a.current,
		{VideoID: "next", Title: "Next", Artist: "Next Artist", RequestedBy: "bob"},
	}
	a.queue.index = 0

	a.syncOBSWidget()
	w := httptest.NewRecorder()
	a.obs.handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/state", nil))

	var got obsWidgetState
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := obsWidgetState{
		Title: "Now", Artist: "Artist", RequestedBy: "ana",
		NextTitle: "Next", NextArtist: "Next Artist", NextRequester: "bob",
	}
	if got != want {
		t.Fatalf("widget state = %+v, want %+v", got, want)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("cache control = %q", w.Header().Get("Cache-Control"))
	}
}

func TestOBSWidgetServesTransparentOverlayAndRejectsOtherPaths(t *testing.T) {
	widget := newOBSWidget()
	handler := widget.handler()
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/", nil))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "background:transparent") ||
		!strings.Contains(page.Body.String(), "Ya viene") || !strings.Contains(page.Body.String(), `textContent=value`) {
		t.Fatalf("unexpected widget page: code %d, body %s", page.Code, page.Body.String())
	}

	notFound := httptest.NewRecorder()
	handler.ServeHTTP(notFound, httptest.NewRequest(http.MethodGet, "/anything", nil))
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("unknown path status = %d, want 404", notFound.Code)
	}
}

func TestOBSWidgetDoesNotPublishWhenDisabled(t *testing.T) {
	a := newTestApp(t)
	a.current = youtube.MusicItem{Title: "Now"}
	a.syncOBSWidget()
	if a.obs.state.Title != "" {
		t.Fatalf("disabled widget state = %+v", a.obs.state)
	}
}
