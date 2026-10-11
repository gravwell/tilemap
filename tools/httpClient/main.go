// httpClient builds a tilemap for a single zoom level by requesting every
// tile directly from a tile server.
//
// Usage:
//
//	httpClient -endpoint http://tiles.example.com/tile/{z}/{x}/{y}.png -zoom 8 -output /data/tiles -parallelism 16
//
// The endpoint may contain {z}, {x} and {y} placeholders.  If it has none the
// conventional "/{z}/{x}/{y}.png" suffix is appended.  Output is written to
// <output>/<zoom>.tiles, the layout the webserver tool expects.  Re-running
// against an existing tilemap resumes, skipping tiles that are already stored.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
)

var (
	fEndpoint    = flag.String("endpoint", "", "Tile server endpoint, may contain {z}, {x} and {y} placeholders (required)")
	fZoom        = flag.Int("zoom", -1, "Zoom level to generate (required)")
	fOutput      = flag.String("output", "", "Output directory for <zoom>.tiles (required)")
	fParallelism = flag.Int("parallelism", 8, "Number of concurrent requests")
	fTimeout     = flag.Duration("timeout", 2*time.Minute, "Per request timeout")
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
	}
	tmpl, err := parseEndpoint(*fEndpoint, *fZoom)
	if err != nil {
		log.Fatalf("Invalid endpoint: %v\n", err)
	}
	if err = os.MkdirAll(*fOutput, 0750); err != nil {
		log.Fatalf("Failed to create output directory: %v\n", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ftch := newFetcher(tmpl, *fParallelism, *fTimeout, *fRetries, *fUserAgent, *fAllowMiss)
	log.Printf("Checking %s\n", tmpl.url(0, 0))
	if err = ftch.check(ctx); err != nil {
		log.Fatalf("Tile server check failed: %v\n", err)
	}

	fname := filepath.Join(*fOutput, fmt.Sprintf("%d.tiles", *fZoom))
	tm, err := tilemap.NewTilemap(fname, *fZoom, false)
	if err != nil {
		log.Fatalf("Failed to open tilemap %s: %v\n", fname, err)
	}

	dim := 1 << uint(*fZoom)
	total := uint64(dim) * uint64(dim)
	log.Printf("Fetching zoom %d (%d tiles) into %s with %d workers\n", *fZoom, total, fname, *fParallelism)

	var st stats
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

// run drives the workers and returns the first fatal error.
func run(pctx context.Context, tm *tilemap.Tilemap, ftch *fetcher, dim, workers int, resume bool, st *stats) error {
	ctx, cancel := context.WithCancel(pctx)
	defer cancel()

	coords := make(chan coord, workers*4)
	errCh := make(chan error, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			if err := worker(ctx, tm, ftch, coords, resume, st); err != nil {
				// a cancellation is a side effect of someone else's
				// failure (or an interrupt), only report real errors
				if !errors.Is(err, context.Canceled) {
					errCh <- err
				}
				cancel() // stop the feeder and the other workers
			}
		}()
	}

	// progress reporter
	total := uint64(dim) * uint64(dim)
	progressDone := make(chan struct{})
	go func() {
		defer close(progressDone)
		report(ctx, st, total)
	}()

	feed(ctx, dim, coords)
	close(coords)
	wg.Wait()
	cancel()
	<-progressDone

	select {
	case err := <-errCh:
		return err
	default:
	}
	return pctx.Err() // nil unless we were interrupted
}

// feed emits every coordinate in the zoom level, walking metatile blocks so
// neighbouring tiles are requested together.
func feed(ctx context.Context, dim int, out chan<- coord) {
	for bx := 0; bx < dim; bx += metatileDim {
		for by := 0; by < dim; by += metatileDim {
			for x := bx; x < bx+metatileDim && x < dim; x++ {
				for y := by; y < by+metatileDim && y < dim; y++ {
					select {
					case out <- coord{x: x, y: y}:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}
}

func worker(ctx context.Context, tm *tilemap.Tilemap, ftch *fetcher, in <-chan coord, resume bool, st *stats) error {
	buf := ftch.newBuffer()
	for c := range in {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if resume {
			if ok, err := tm.Has(c.x, c.y); err != nil {
				return err
			} else if ok {
				st.skipped.Add(1)
				continue
			}
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

func fatalUsage(format string, args ...interface{}) {
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
