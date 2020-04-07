package main

import (
	"flag"
	"fmt"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	tilemap "../.."

	"github.com/fawick/go-mapnik/mapnik"
)

var (
	fTileFile  = flag.String("map-file", ``, "Path to mapnic render file XML")
	fFontsPath = flag.String("fonts-dir", `/usr/share/fonts/truetype/dejavu,/usr/share/fonts/truetype/noto,/usr/share/fonts/truetype/unifont`,
		"Register fonts directories using a comma delimited list")
	fThreads = flag.Int("threads", 4, "Number of threads to use")
	fZooms   = flag.String("zooms", "0-15", "Zoom levels to generate")
	fTileDir = flag.String("tile-dir", `/tmp/tiles`, "Path to output for tilemaps")
)

func main() {
	var err error
	var zooms []uint64
	flag.Parse()
	if zooms, err = parseZooms(*fZooms); err != nil {
		log.Fatal("Failed to parse zooms", err)
	}

	if *fFontsPath != `` {
		bits := strings.Split(*fFontsPath, ",")
		for _, v := range bits {
			mapnik.RegisterFonts(v)
		}
	}
	log.Println("Fonts registered")

	tms := make(map[uint64]*tilemap.Tilemap, len(zooms))
	for _, z := range zooms {
		fname := filepath.Join(*fTileDir, fmt.Sprintf("%d.tiles", z))
		wtr, err := tilemap.NewTilemap(fname, int(z), false)
		if err != nil {
			log.Fatal(err)
		}
		tms[z] = wtr
	}

	reqChan := make(chan renderreq, *fThreads+1)
	resChan := make(chan rendered, *fThreads*4)

	wwg := sync.WaitGroup{}
	wwg.Add(1)
	go func(wg *sync.WaitGroup) {
		if err = writer(tms, resChan); err != nil {
			log.Fatal("Worker error", err)
		}
		wg.Done()
	}(&wwg)
	log.Println("Writer started")

	twg := sync.WaitGroup{}
	twg.Add(*fThreads)
	for i := 0; i < *fThreads; i++ {
		time.Sleep(time.Second)
		w, err := NewWorker(*fTileFile, i)
		if err != nil {
			log.Fatal("Failed to make worker", i)
		}
		go w.run(reqChan, resChan, &twg)
		log.Printf("Fired thread %d/%d\n", i+1, *fThreads)
	}

	log.Println("Feeding zooms")
	for _, z := range zooms {
		dim := 1 << z
		for i := 0; i < dim; i++ {
			for j := 0; j < dim; j++ {
				reqChan <- renderreq{
					zoom: z,
					x:    uint64(i),
					y:    uint64(j),
				}
			}
		}
	}
	close(reqChan)
	fmt.Println("\nRequest loop done")
	twg.Wait()

	close(resChan)
	wwg.Wait()

	for _, v := range tms {
		if err = v.Close(); err != nil {
			log.Fatal(err)
		}
	}
}

type renderreq struct {
	zoom, x, y uint64
}

type rendered struct {
	zoom uint
	x, y uint
	buff []byte
	err  error
}

func writer(tms map[uint64]*tilemap.Tilemap, in <-chan rendered) error {
	var cnt uint64
	var total uint64
	var lastz uint
	var dur time.Duration
	tckr := time.NewTicker(3 * time.Second)
	defer tckr.Stop()
	dtckr := time.NewTicker(10 * time.Second)
	defer dtckr.Stop()
	ts := time.Now()

	var lastcnt uint64
	lastchk := time.Now()

consume:
	for {
		select {
		case v, ok := <-in:
			if !ok {
				break consume
			}
			if v.err != nil {
				return v.err
			}
			tm, ok := tms[uint64(v.zoom)]
			if !ok {
				return fmt.Errorf("got render result for invalid writer %d", v.zoom)
			}
			if err := tm.Add(int(v.x), int(v.y), v.buff); err != nil {
				return err
			}
			if v.zoom != lastz {
				log.Printf("\nZoom level %d with %d tiles done in %v\n",
					lastz, total, time.Since(ts))
				lastz = v.zoom
				total = (1 << v.zoom) * (1 << v.zoom)
				cnt = 0
				ts = time.Now()
			} else {
				if total == 0 {
					total = (1 << v.zoom) * (1 << v.zoom)
				}
				cnt++
			}
		case <-tckr.C:
			if dur > 0 {
				fmt.Printf("\r%d %d %d %v            ", lastz, cnt, total, dur)
			} else {
				fmt.Printf("\r%d %d %d               ", lastz, cnt, total)
			}
		case <-dtckr.C:
			if cnt > 0 && total > 0 {
				diff := cnt - lastcnt
				secs := float64(time.Since(lastchk)) / float64(time.Second)
				tps := float64(diff) / secs
				secsToComplete := float64(total-cnt) / tps
				dur = time.Duration(secsToComplete) * time.Second
			}
			lastchk = time.Now()
			lastcnt = cnt
		}
	}
	fmt.Println("\nWriter done")
	return nil
}

func parseZooms(s string) (r []uint64, err error) {
	var minS, maxS string
	var min, max uint64
	bits := strings.Split(s, `-`)
	if len(bits) == 1 {
		minS = strings.TrimSpace(bits[0])
		maxS = minS
	} else if len(bits) == 2 {
		minS = strings.TrimSpace(bits[0])
		maxS = strings.TrimSpace(bits[1])
	} else {
		err = fmt.Errorf("Invalid zoom ranges")
		return
	}
	if min, err = strconv.ParseUint(minS, 10, 8); err != nil {
		return
	} else if max, err = strconv.ParseUint(maxS, 10, 8); err != nil {
		return
	}
	if min > max {
		err = fmt.Errorf("Range is in invalid %d > %d", min, max)
		return
	} else if min > tilemap.MaxZoom || max > tilemap.MaxZoom {
		err = fmt.Errorf("zoom range is invalid, must be less than %d", tilemap.MaxZoom)
		return
	}
	for i := min; i <= max; i++ {
		r = append(r, i)
	}
	return
}
