package main

import (
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"sync"
)

const obsWidgetURL = "http://127.0.0.1:38121/"

type obsWidgetState struct {
	Title         string `json:"title"`
	Artist        string `json:"artist"`
	RequestedBy   string `json:"requestedBy"`
	NextTitle     string `json:"nextTitle"`
	NextArtist    string `json:"nextArtist"`
	NextRequester string `json:"nextRequester"`
}

type obsWidget struct {
	mu       sync.RWMutex
	state    obsWidgetState
	listener net.Listener
	server   *http.Server
	err      string
}

func newOBSWidget() *obsWidget {
	return &obsWidget{}
}

func (w *obsWidget) start() error {
	w.mu.Lock()
	if w.server != nil {
		w.mu.Unlock()
		return nil
	}
	w.mu.Unlock()

	listener, err := net.Listen("tcp", "127.0.0.1:38121")
	if err != nil {
		w.mu.Lock()
		w.err = err.Error()
		w.mu.Unlock()
		return err
	}
	server := &http.Server{Handler: w.handler()}
	w.mu.Lock()
	w.listener, w.server, w.err = listener, server, ""
	w.mu.Unlock()
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("OBS widget server: %v", err)
			w.mu.Lock()
			w.err = err.Error()
			w.mu.Unlock()
		}
	}()
	return nil
}

func (w *obsWidget) stop() {
	w.mu.Lock()
	server := w.server
	w.server, w.listener = nil, nil
	w.mu.Unlock()
	if server != nil {
		if err := server.Close(); err != nil {
			log.Printf("stopping OBS widget server: %v", err)
		}
	}
}

func (w *obsWidget) publish(state obsWidgetState) {
	w.mu.Lock()
	w.state = state
	w.mu.Unlock()
}

// syncOBSWidget publishes the current and next queue entries without exposing
// app state to the HTTP-serving goroutine.
func (a *app) syncOBSWidget() {
	if a.obs == nil || !a.settings.OBSWidget {
		return
	}
	state := obsWidgetState{
		Title:       a.current.Title,
		Artist:      a.current.Artist,
		RequestedBy: a.current.RequestedBy,
	}
	if _, next, ok := a.following(); ok {
		state.NextTitle = next.Title
		state.NextArtist = next.Artist
		state.NextRequester = next.RequestedBy
	}
	a.obs.publish(state)
}

func (a *app) setOBSWidget(enabled bool) {
	a.settings.OBSWidget = enabled
	if enabled {
		if err := a.obs.start(); err != nil {
			log.Printf("starting OBS widget: %v", err)
		}
		a.syncOBSWidget()
	} else {
		a.obs.stop()
		a.obs.publish(obsWidgetState{})
	}
	a.saveSettings()
}

func (w *obsWidget) status() (bool, string) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.server != nil, w.err
}

func (w *obsWidget) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(rw, r)
			return
		}
		rw.Header().Set("Content-Type", "text/html; charset=utf-8")
		rw.Header().Set("Cache-Control", "no-store")
		rw.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = rw.Write([]byte(obsWidgetHTML))
	})
	mux.HandleFunc("GET /state", func(rw http.ResponseWriter, _ *http.Request) {
		w.mu.RLock()
		state := w.state
		w.mu.RUnlock()
		rw.Header().Set("Content-Type", "application/json; charset=utf-8")
		rw.Header().Set("Cache-Control", "no-store")
		rw.Header().Set("X-Content-Type-Options", "nosniff")
		if err := json.NewEncoder(rw).Encode(state); err != nil {
			log.Printf("writing OBS widget state: %v", err)
		}
	})
	return mux
}

const obsWidgetHTML = `<!doctype html>
<html lang="es"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<style>
*{box-sizing:border-box}html,body{margin:0;width:100%;height:100%;background:transparent;color:#fff;font-family:Inter,"Segoe UI",Arial,sans-serif}
.wrap{display:flex;align-items:flex-end;gap:14px;width:100%;height:100%;padding:12px}
.now,.next{background:rgba(15,18,26,.88);border:1px solid rgba(255,255,255,.16);border-radius:14px;box-shadow:0 6px 24px #0005;backdrop-filter:blur(10px);min-width:0}
.now{padding:14px 18px;flex:1}.eyebrow{font-size:11px;font-weight:800;letter-spacing:.16em;text-transform:uppercase;color:#b9a2ff;margin-bottom:6px}
.title{font-size:clamp(18px,3vw,30px);line-height:1.12;font-weight:750;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.byline{font-size:14px;color:#d0d2dc;margin-top:5px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.next{padding:10px 14px;max-width:38%;align-self:flex-end}.next .eyebrow{font-size:9px;color:#91d7ff;margin-bottom:4px}.next .title{font-size:clamp(13px,1.8vw,18px)}.next .byline{font-size:11px;margin-top:3px}
.empty{opacity:.55}
</style></head><body><main class="wrap">
<section class="now"><div class="eyebrow">Ahora suena</div><div class="title" id="title">Meiro</div><div class="byline" id="byline"></div></section>
<section class="next"><div class="eyebrow">Ya viene</div><div class="title" id="nextTitle">—</div><div class="byline" id="nextByline"></div></section>
</main><script>
const put=(id,value)=>{document.getElementById(id).textContent=value||""};
async function update(){try{const r=await fetch("/state",{cache:"no-store"});if(!r.ok)return;const s=await r.json();put("title",s.title||"Meiro");put("byline",[s.artist,s.requestedBy?"Pedida por "+s.requestedBy:""].filter(Boolean).join(" · "));put("nextTitle",s.nextTitle||"—");put("nextByline",[s.nextArtist,s.nextRequester?"Pidió "+s.nextRequester:""].filter(Boolean).join(" · "))}catch{}}
update();setInterval(update,1000);
</script></body></html>`
