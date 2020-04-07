package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	tilemap "../.."
	"github.com/gorilla/mux"
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
	lst net.Listener
	tm  []*tilemap.Tilemap
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
	rtr := mux.NewRouter()
	rtr.HandleFunc(`/tiles/{zoom:\d+}/{x:\d+}/{y:\d+}.png`, w.tileHandler).Methods(`GET`)
	if c.FileDir != `` {
		rtr.NotFoundHandler = fhandler{http.FileServer(http.Dir(filepath.Clean(c.FileDir)))}
	}
	w.Server.Handler = rtr

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
	return
}

type fhandler struct {
	h http.Handler
}

func (h fhandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.h.ServeHTTP(w, r)
}
