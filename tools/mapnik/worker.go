package main

import (
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"github.com/fawick/go-mapnik/mapnik"
)

const (
	mapSize  = 256
	buffSize = 256

	restDur = 5 * time.Second
)

type worker struct {
	sheet string
	id    int
	m     *mapnik.Map
	mp    mapnik.Projection
}

func NewWorker(tileFile string, id int) (w *worker, err error) {
	m := mapnik.NewMap(mapSize, mapSize)
	if err = m.Load(tileFile); err != nil {
		return
	}
	p := m.Projection()
	m.Resize(mapSize, mapSize)
	m.SetBufferSize(buffSize)

	w = &worker{
		sheet: tileFile,
		id:    id,
		m:     m,
		mp:    p,
	}
	return
}

func (w *worker) run(in <-chan renderreq, out chan rendered, wg *sync.WaitGroup) {
	if err := w.loop(in, out); err != nil {
		wg.Done()
		w.Close()
		log.Fatalf("Thread %d failed with %v\n", w.id, err)
	} else {
		fmt.Printf("Thread %d done\n", w.id)
	}
	wg.Done()
	w.Close()
}

func (w *worker) loop(in <-chan renderreq, out chan rendered) (err error) {
	for v := range in {
		r := rendered{
			zoom: uint(v.zoom),
			x:    uint(v.x),
			y:    uint(v.y),
		}
		if r.buff, r.err = w.render(v.zoom, v.x, v.y); r.err != nil {
			err = r.err
			break
		}
		out <- r
	}
	return
}

func (w *worker) render(zoom, x, y uint64) (buff []byte, err error) {
	for {
		if buff, err = w.renderZXY(zoom, x, y); err == nil {
			return
		}
		//something failed
		log.Printf("Worker %d Failed to render %d/%d/%d %v\n", w.id, zoom, x, y, err)
		if err = w.reload(restDur); err != nil {
			return
		}
		log.Printf("%d reloaded\n", w.id)
	}
	return
}

func (w *worker) renderZXY(zoom, x, y uint64) (buff []byte, err error) {
	// Calculate pixel positions of bottom left & top right
	p0 := [2]float64{float64(x) * mapSize, (float64(y) + 1) * mapSize}
	p1 := [2]float64{(float64(x) + 1) * mapSize, float64(y) * mapSize}

	// Convert to LatLong(EPSG:4326)
	l0 := fromPixelToLL(p0, zoom)
	l1 := fromPixelToLL(p1, zoom)

	// Convert to map projection (e.g. mercartor co-ords EPSG:3857)
	c0 := w.mp.Forward(mapnik.Coord{l0[0], l0[1]})
	c1 := w.mp.Forward(mapnik.Coord{l1[0], l1[1]})

	// Bounding box for the Tile
	w.m.ZoomToMinMax(c0.X, c0.Y, c1.X, c1.Y)

	if buff, err = w.m.RenderToMemoryPng(); err != nil {
		buff = nil
	}
	return
}

func (w *worker) reload(rest time.Duration) (err error) {
	w.Close()
	time.Sleep(rest)
	m := mapnik.NewMap(mapSize, mapSize)
	if err = m.Load(w.sheet); err != nil {
		return
	}
	p := m.Projection()
	m.Resize(mapSize, mapSize)
	m.SetBufferSize(buffSize)
	w.m = m
	w.mp = p
	time.Sleep(rest)
	return
}

func (w *worker) Close() {
	w.m.Free()
	w.mp.Free()
	return
}

// This has been reimplemented based on OpenStreetMap generate_tiles.py
// ripped right out of maptiles
func minmax(a, b, c float64) float64 {
	a = math.Max(a, b)
	a = math.Min(a, c)
	return a
}

var gp struct {
	Bc []float64
	Cc []float64
	zc [][2]float64
	Ac []float64
}

func init() {
	c := 256.0
	for d := 0; d < 30; d++ {
		e := c / 2
		gp.Bc = append(gp.Bc, c/360.0)
		gp.Cc = append(gp.Cc, c/(2*math.Pi))
		gp.zc = append(gp.zc, [2]float64{e, e})
		gp.Ac = append(gp.Ac, c)
		c *= 2
	}
}

func fromLLtoPixel(ll [2]float64, zoom uint64) [2]float64 {
	d := gp.zc[zoom]
	e := math.Trunc((d[0] + ll[0]*gp.Bc[zoom]) + 0.5)
	f := minmax(math.Sin(ll[1]*math.Pi/180.0), -0.9999, 0.9999)
	g := math.Trunc((d[1] + 0.5*math.Log((1+f)/(1-f))*-gp.Cc[zoom]) + 0.5)
	return [2]float64{e, g}
}

func fromPixelToLL(px [2]float64, zoom uint64) [2]float64 {
	e := gp.zc[zoom]
	f := (px[0] - e[0]) / gp.Bc[zoom]
	g := (px[1] - e[1]) / -gp.Cc[zoom]
	h := 180.0 / math.Pi * (2*math.Atan(math.Exp(g)) - 0.5*math.Pi)
	return [2]float64{f, h}
}
