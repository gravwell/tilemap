package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/gravwell/tilemap/v1"
)

const (
	maxZoom = 20
)

var (
	ErrBadRequest = errors.New("bad request")
)

type Webserver struct {
	Config
	http.Server
	sync.WaitGroup
	lst  net.Listener
	tm   []*tilemap.Tilemap
	lwtr io.WriteCloser
}

func NewWebserver(c Config, maps []*tilemap.Tilemap) (w *Webserver, err error) {
	var lst net.Listener
	if lst, err = net.Listen("tcp", c.BindString()); err != nil {
		return
	}

	w = &Webserver{
		Config: c,
		lst:    lst,
		tm:     maps,
		Server: http.Server{
			WriteTimeout: 5 * time.Second, //these are tiny files, so this is even kind of nuts
			ReadTimeout:  time.Second,
		},
	}
	if w.lwtr, err = c.LogWriter(); err != nil {
		return
	}
	rtr := mux.NewRouter()
	rtr.HandleFunc(`/tiles/{zoom:\d+}/{x:\d+}/{y:\d+}.png`, w.tileHandler).Methods(`GET`)
	if c.FileDir != `` {
		rtr.NotFoundHandler = fhandler{http.FileServer(http.Dir(filepath.Clean(c.FileDir)))}
	}
	w.Server.Handler = loggingHandler(w.lwtr, rtr)

	return
}

func (ws *Webserver) tileHandler(w http.ResponseWriter, r *http.Request) {
	zoom, x, y, err := getTileVars(r)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
	} else if zoom < 0 || zoom >= len(ws.tm) || ws.tm[zoom] == nil {
		w.WriteHeader(http.StatusNotFound)
	} else if tbuff, err := ws.tm[zoom].GetTile(x, y); err != nil {
		fmt.Println("Get Tile", zoom, x, y, err)
		w.WriteHeader(http.StatusInternalServerError)
	} else {
		w.Header().Set("Content-Type", "image/png")
		w.Write(tbuff)
	}
}

func getTileVars(r *http.Request) (zoom, x, y int, err error) {
	if r == nil {
		err = ErrBadRequest
		return
	}
	mp := mux.Vars(r)
	if mp == nil {
		err = ErrBadRequest
		return
	}
	if st, ok := mp[`zoom`]; !ok {
		err = ErrBadRequest
		return
	} else if zoom, err = strconv.Atoi(st); err != nil {
		return
	}

	if st, ok := mp[`x`]; !ok {
		err = ErrBadRequest
		return
	} else if x, err = strconv.Atoi(st); err != nil {
		return
	}
	if st, ok := mp[`y`]; !ok {
		err = ErrBadRequest
		return
	} else if y, err = strconv.Atoi(st); err != nil {
		return
	}
	if zoom < 0 || zoom > maxZoom || x < 0 || y < 0 {
		err = ErrBadRequest
	}
	return
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
	err = w.lst.Close()
	w.Wait()
	if err == nil {
		err = w.lwtr.Close()
	} else {
		w.lwtr.Close()
	}
	return
}

type fhandler struct {
	h http.Handler
}

func (h fhandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.h.ServeHTTP(w, r)
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
