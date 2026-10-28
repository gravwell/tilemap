package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gravwell/tilemap"
)

const (
	maxZoom = 20
)

var (
	ErrBadRequest = errors.New("bad request")

	errNoMatch = errors.New("path is not a tile request")
)

type Webserver struct {
	Config
	http.Server
	sync.WaitGroup
	lst  net.Listener
	tm   []*tilemap.Tilemap
	fbh  http.Handler // handles requests that are not tile requests
	accW io.WriteCloser
	lgrW io.WriteCloser
	lgr  *log.Logger
}

func NewWebserver(c Config, maps []*tilemap.Tilemap) (w *Webserver, err error) {
	var lst net.Listener
	if lst, err = net.Listen("tcp", c.BindString()); err != nil {
		return
	}

	w = &Webserver{
		Config:       c,
		lst:          lst,
		tm:           maps,
		WriteTimeout: 5 * time.Second, //these are tiny files, so this is even kind of nuts
		ReadTimeout:  time.Second,
	}
	if w.accW, err = c.AccessLogWriter(); err != nil {
		return
	}
	if w.lgrW, err = c.LogWriter(); err != nil {
		return
	}
	w.lgr = log.New(w.lgrW, ``, log.LUTC|log.Lshortfile|log.LstdFlags)
	rtr := http.NewServeMux()
	rtr.HandleFunc(`GET /tiles/{zoom}/{x}/{y}`, w.tileHandler)
	if c.FileDir != `` {
		w.fbh = http.FileServer(http.Dir(filepath.Clean(c.FileDir)))
		rtr.Handle(`/`, w.fbh)
	} else {
		w.fbh = http.NotFoundHandler()
	}
	w.Server.Handler = loggingHandler(w.accW, rtr)
	w.Server.ErrorLog = w.lgr

	return
}

func (ws *Webserver) tileHandler(w http.ResponseWriter, r *http.Request) {
	zoom, x, y, err := getTileVars(r)
	if errors.Is(err, errNoMatch) {
		// the stdlib mux can't express the numeric/.png constraints, so hand
		// anything that doesn't look like a tile off to the fallback handler
		ws.fbh.ServeHTTP(w, r)
	} else if err != nil {
		w.WriteHeader(http.StatusBadRequest)
	} else if zoom < 0 || zoom >= len(ws.tm) || ws.tm[zoom] == nil {
		w.WriteHeader(http.StatusNotFound)
	} else if tbuff, err := ws.tm[zoom].GetTile(x, y); err != nil {
		if errors.Is(err, tilemap.ErrTileNotFound) || errors.Is(err, tilemap.ErrInvalidTileID) {
			w.WriteHeader(http.StatusNotFound)
		} else {
			w.WriteHeader(http.StatusInternalServerError)
			ws.lgr.Printf("ERROR GetTile %d/%d/%d - %v\n", zoom, x, y, err)
		}
	} else {
		h := w.Header()
		h.Set("Content-Type", "image/png")
		h.Set("Content-Length", strconv.Itoa(len(tbuff)))
		w.Write(tbuff)
	}
}

func getTileVars(r *http.Request) (zoom, x, y int, err error) {
	if r == nil {
		err = ErrBadRequest
		return
	}
	zs, xs := r.PathValue(`zoom`), r.PathValue(`x`)
	ys, ok := strings.CutSuffix(r.PathValue(`y`), `.png`)
	if !ok || !isDigits(zs) || !isDigits(xs) || !isDigits(ys) {
		err = errNoMatch
		return
	}
	if zoom, err = strconv.Atoi(zs); err != nil {
		return
	} else if x, err = strconv.Atoi(xs); err != nil {
		return
	} else if y, err = strconv.Atoi(ys); err != nil {
		return
	}
	if zoom > maxZoom {
		err = ErrBadRequest
	}
	return
}

// isDigits reports whether s is a non-empty string of ASCII digits
func isDigits(s string) bool {
	if s == `` {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (w *Webserver) Start() (err error) {
	if w.lst == nil {
		err = errors.New("not ready")
	} else {
		w.Add(1)
		go w.run()
	}

	return
}

func (w *Webserver) run() {
	defer w.Done()
	w.Serve(w.lst)
}

func (w *Webserver) Close() (err error) {
	if err = w.lst.Close(); err != nil {
		w.lgr.Printf("ERROR Failed to close listener: %v\n", err)
	}
	w.Wait()
	w.accW.Close()
	w.lgrW.Close()
	return
}

type logHandler struct {
	sync.Mutex
	wtr  io.Writer
	hndr http.Handler
}

func loggingHandler(wtr io.Writer, hndr http.Handler) http.Handler {
	return &logHandler{
		wtr:  wtr,
		hndr: hndr,
	}
}

func (lh *logHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	wt := &writeTracker{w: w}

	ts := time.Now()
	lh.hndr.ServeHTTP(wt, req)
	if req.MultipartForm != nil {
		req.MultipartForm.RemoveAll()
	}
	dur := time.Since(ts)
	lh.log(req, wt, ts, dur)
}

func (lh *logHandler) log(r *http.Request, wt *writeTracker, ts time.Time, dur time.Duration) {
	ms := dur.Seconds() * 1000.0
	host := getRemoteHost(r)
	ln := fmt.Sprintf("%v\t%s\t%s\t%s\t%d\t%d\t%q\t%.2f\n", ts.UTC().Format(time.RFC3339Nano),
		host, r.Method, r.URL.String(), wt.resp, wt.size, getUserAgent(r), ms)
	lh.Lock()
	io.WriteString(lh.wtr, ln)
	lh.Unlock()
}

func getUserAgent(r *http.Request) string {
	return r.Header.Get(`User-Agent`)
}

func getRemoteHost(r *http.Request) string {
	//check for nil first
	if r == nil {
		return ``
	}
	//check if the request was forwarded and has an X-Forwarded-Host header
	if r.Header != nil {
		if s := r.Header.Get(`X-Forwarded-Host`); len(s) > 0 {
			if h, _, err := net.SplitHostPort(s); err == nil {
				return h
			}
			return s
		}
	}
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

type writeTracker struct {
	w    http.ResponseWriter
	size int
	resp int
}

func (wt *writeTracker) Header() http.Header {
	return wt.w.Header()
}

func (wt *writeTracker) WriteHeader(s int) {
	wt.resp = s
	wt.w.WriteHeader(s)
}

func (wt *writeTracker) Write(b []byte) (n int, err error) {
	n, err = wt.w.Write(b)
	wt.size += n
	if wt.resp == 0 {
		wt.resp = 200
	}
	return
}
