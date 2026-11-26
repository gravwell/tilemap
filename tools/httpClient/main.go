// httpClient builds a tilemap for a single zoom level by requesting every
// tile directly from a tile server.
//
// Usage:
//
//	httpClient -endpoint http://tiles.example.com/tile/{z}/{x}/{y}.png -zoom 8 -output /data/tiles -parallelism 16
//
// The endpoint may contain {z}, {x} and {y} placeholders.  If it has none the
// conventional "/{z}/{x}/{y}.png" suffix is appended.  Output is written to
// <output>/<zoom>.tiles, the layout the webserver tool expects.
//
// Runs can be stopped and restarted.  An existing tilemap is scanned before
// anything is fetched: its index must match the requested zoom level, and
// every tile it already holds is skipped, so only the missing tiles are
// requested.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gravwell/tilemap"
)

const (
	// metatileDim is the block size used to order requests.  Rendering
	// backends such as renderd generate 8x8 metatiles, so asking for all
	// tiles in a block back to back gets the remaining 63 out of its cache.
	metatileDim = 8

	progressInterval = 5 * time.Second

	// flushInterval bounds how much work a hard kill can lose.  Fetched
	// tiles sit in the tilemap's write buffer until it fills, which for
	// small tiles from a slow renderer can be many minutes of requests.
	flushInterval = 10 * time.Second
)

var (
	fEndpoint    = flag.String("endpoint", "", "Tile server endpoint, may contain {z}, {x} and {y} placeholders (required)")
	fZoom        = flag.Int("zoom", -1, "Zoom level to generate (required)")
	fOutput      = flag.String("output", "", "Output directory for <zoom>.tiles (required)")
	fParallelism = flag.Int("parallelism", 8, "Number of concurrent requests")
	fTimeout     = flag.Duration("timeout", 2*time.Minute, "Per request timeout")
	fCheckTime   = flag.Duration("check-timeout", 30*time.Second, "Timeout for the startup request that checks the endpoint")
	fRetries     = flag.Int("retries", 10, "Retries per tile on transient failures, 0 retries forever")
	fResume      = flag.Bool("resume", true, "Skip tiles already present in an existing tilemap")
	fAllowMiss   = flag.Bool("allow-missing", false, "Skip tiles the server answers 404 for instead of failing")
	fUserAgent   = flag.String("user-agent", "gravwell-tilemap/1.0", "User-Agent header to send")
)

type stats struct {
	fetched  atomic.Uint64 // tiles pulled from the server and stored
	skipped  atomic.Uint64 // tiles already in the tilemap
	missing  atomic.Uint64 // tiles the server reported as 404
	retries  atomic.Uint64 // transient failures that were retried
	netBytes atomic.Uint64 // bytes received from the server
}

func (s *stats) done() uint64 {
	return s.fetched.Load() + s.skipped.Load() + s.missing.Load()
}

type coord struct {
	x, y int
}

func main() {
	flag.Parse()
	if *fEndpoint == `` {
		fatalUsage("endpoint is required")
	} else if *fZoom < 0 || *fZoom > tilemap.MaxZoom {
		fatalUsage("zoom must be between 0 and %d", tilemap.MaxZoom)
	} else if *fOutput == `` {
		fatalUsage("output directory is required")
	} else if *fParallelism < 1 {
		fatalUsage("parallelism must be at least 1")
	} else if *fRetries < 0 {
		fatalUsage("retries must not be negative")
	} else if *fCheckTime <= 0 {
		fatalUsage("check-timeout must be positive")
	}
	tmpl, err := parseEndpoint(*fEndpoint, *fZoom)
	if err != nil {
		log.Fatalf("Invalid endpoint: %v\n", err)
	}
	if err = os.MkdirAll(*fOutput, 0750); err != nil {
		log.Fatalf("Failed to create output directory: %v\n", err)
	}

	dim := 1 << uint(*fZoom)
	total := uint64(dim) * uint64(dim)
	fname := filepath.Join(*fOutput, fmt.Sprintf("%d.tiles", *fZoom))

	// validate any existing tilemap before it is opened for writing, a
	// mismatched file would be grown and have its contents treated as index.
	// This runs before the interrupt handler so a long scan can be killed.
	present, err := existingTiles(fname, *fZoom)
	if err != nil {
		log.Fatalf("Cannot resume: %v\n", err)
	}
	if !*fResume {
		present = 0 // fetch everything again
	} else if present == total {
		log.Printf("%s already holds all %d tiles, nothing to do\n", fname, total)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ftch := newFetcher(tmpl, *fParallelism, *fTimeout, *fRetries, *fUserAgent, *fAllowMiss)
	log.Printf("Checking %s\n", tmpl.url(0, 0))
	if err = ftch.check(ctx, *fCheckTime); err != nil {
		log.Fatalf("Tile server check failed: %v\n", err)
	}

	tm, err := tilemap.NewTilemap(fname, *fZoom, false)
	if err != nil {
		log.Fatalf("Failed to open tilemap %s: %v\n", fname, err)
	}

	log.Printf("Fetching zoom %d (%d of %d tiles remaining) into %s with %d workers\n",
		*fZoom, total-present, total, fname, *fParallelism)

	var st stats
	st.skipped.Store(present) // counted up front so the rate only reflects real work
	startSize := tm.Size()
	start := time.Now()
	err = run(ctx, tm, ftch, dim, *fParallelism, *fResume, &st)
	elapsed := time.Since(start)

	// always close so the tilemap is consistent even on an interrupted run
	if cerr := tm.Close(); cerr != nil {
		log.Printf("Failed to close tilemap: %v\n", cerr)
		if err == nil {
			err = cerr
		}
	}

	fmt.Printf("\n")
	log.Printf("Done %d/%d tiles in %v: %d fetched, %d already present, %d missing on server, %d retries\n",
		st.done(), total, elapsed.Round(time.Millisecond), st.fetched.Load(), st.skipped.Load(),
		st.missing.Load(), st.retries.Load())
	if fetched := st.netBytes.Load(); fetched > 0 {
		grew := fileSize(fname) - startSize
		log.Printf("Received %s, tilemap grew %s (%.1f%% deduplicated)\n",
			humanBytes(fetched), humanBytes(uint64(grew)), 100*(1-float64(grew)/float64(fetched)))
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			log.Printf("Interrupted, re-run with the same arguments to resume\n")
		} else {
			log.Printf("Failed: %v\n", err)
		}
		os.Exit(1)
	}
}

// existingTiles validates the tilemap at fname, if there is one, and returns
// how many tiles it already holds.  A missing or empty file holds none.
func existingTiles(fname string, zoom int) (uint64, error) {
	fi, err := os.Stat(fname)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, err
	} else if !fi.Mode().IsRegular() {
		return 0, fmt.Errorf("%s is not a regular file", fname)
	} else if fi.Size() == 0 {
		return 0, nil
	}
	log.Printf("Scanning existing tilemap %s (%s)\n", fname, humanBytes(uint64(fi.Size())))
	start := time.Now()
	info, err := tilemap.ScanIndex(fname, zoom)
	if err != nil {
		return 0, fmt.Errorf("%s is not a valid zoom %d tilemap, move it aside to start over: %w", fname, zoom, err)
	}
	log.Printf("Found %d/%d tiles (%.1f%%) in %v\n", info.Present, info.Tiles,
		100*float64(info.Present)/float64(info.Tiles), time.Since(start).Round(time.Millisecond))
	return info.Present, nil
}

// run drives the workers and returns the first fatal error.  When resume is
// set, tiles already in tm are skipped and never handed to a worker.
func run(pctx context.Context, tm *tilemap.Tilemap, ftch *fetcher, dim, workers int, resume bool, st *stats) error {
	ctx, cancel := context.WithCancel(pctx)
	defer cancel()

	coords := make(chan coord, workers*4)
	errCh := make(chan error, workers+1)
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			if err := worker(ctx, tm, ftch, coords, st); err != nil {
				// a cancellation is a side effect of someone else's
				// failure (or an interrupt), only report real errors
				if !errors.Is(err, context.Canceled) {
					errCh <- err
				}
				cancel() // stop the feeder and the other workers
			}
		}()
	}

	// progress reporter and periodic flusher
	total := uint64(dim) * uint64(dim)
	bgDone := make(chan struct{}, 2)
	go func() {
		report(ctx, st, total)
		bgDone <- struct{}{}
	}()
	go func() {
		if err := flusher(ctx, tm); err != nil {
			errCh <- err
			cancel()
		}
		bgDone <- struct{}{}
	}()

	ferr := feed(ctx, tm, dim, resume, coords)
	close(coords)
	wg.Wait()
	cancel()
	<-bgDone
	<-bgDone

	select {
	case err := <-errCh:
		return err
	default:
	}
	if ferr != nil && !errors.Is(ferr, context.Canceled) {
		return ferr
	}
	return pctx.Err() // nil unless we were interrupted
}

// feed emits every coordinate in the zoom level that still needs fetching,
// walking metatile blocks so neighbouring tiles are requested together.
// With resume set, tiles already present in tm are skipped.
func feed(ctx context.Context, tm *tilemap.Tilemap, dim int, resume bool, out chan<- coord) error {
	for bx := 0; bx < dim; bx += metatileDim {
		for by := 0; by < dim; by += metatileDim {
			for x := bx; x < bx+metatileDim && x < dim; x++ {
				for y := by; y < by+metatileDim && y < dim; y++ {
					if resume {
						if ok, err := tm.Has(x, y); err != nil {
							return fmt.Errorf("tile %d/%d: %w", x, y, err)
						} else if ok {
							continue
						}
					}
					select {
					case out <- coord{x: x, y: y}:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
			}
		}
	}
	return nil
}

func worker(ctx context.Context, tm *tilemap.Tilemap, ftch *fetcher, in <-chan coord, st *stats) error {
	buf := ftch.newBuffer()
	for c := range in {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		buff, err := ftch.get(ctx, buf, c.x, c.y, st)
		if err != nil {
			if errors.Is(err, errNotFound) {
				st.missing.Add(1)
				continue
			}
			return fmt.Errorf("tile %d/%d: %w", c.x, c.y, err)
		}
		if err = tm.Add(c.x, c.y, buff); err != nil {
			return fmt.Errorf("tile %d/%d: %w", c.x, c.y, err)
		}
		st.fetched.Add(1)
	}
	return nil
}

// flusher periodically pushes buffered tiles into the file so they survive
// the process being killed.
func flusher(ctx context.Context, tm *tilemap.Tilemap) error {
	tckr := time.NewTicker(flushInterval)
	defer tckr.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tckr.C:
			if err := tm.Flush(); err != nil {
				return fmt.Errorf("flushing tilemap: %w", err)
			}
		}
	}
}

func report(ctx context.Context, st *stats, total uint64) {
	tckr := time.NewTicker(progressInterval)
	defer tckr.Stop()
	start := time.Now()
	lastDone, lastTs := st.done(), start
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tckr.C:
			done := st.done()
			rate := float64(done-lastDone) / now.Sub(lastTs).Seconds()
			lastDone, lastTs = done, now
			eta := "?"
			if rate > 0 {
				eta = (time.Duration(float64(total-done)/rate) * time.Second).Round(time.Second).String()
			}
			fmt.Printf("\r%d/%d (%.1f%%) %.0f tiles/s %s received, ETA %s          ",
				done, total, 100*float64(done)/float64(total), rate, humanBytes(st.netBytes.Load()), eta)
		}
	}
}

func fatalUsage(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n\n", args...)
	flag.Usage()
	os.Exit(2)
}

func fileSize(pth string) int64 {
	if fi, err := os.Stat(pth); err == nil {
		return fi.Size()
	}
	return 0
}

func humanBytes(v uint64) string {
	const unit = 1024
	if v < unit {
		return fmt.Sprintf("%d B", v)
	}
	div, exp := uint64(unit), 0
	for n := v / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(v)/float64(div), "KMGTPE"[exp])
}
