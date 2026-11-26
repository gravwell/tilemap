package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gravwell/tilemap"
)

// tileServer serves a deterministic, distinct body for every tile of one
// zoom level and records how many times each tile was requested.
type tileServer struct {
	*httptest.Server
	mtx      sync.Mutex
	requests map[coord]int
	total    int
	onReq    func(n int) // called with the running request count
}

func newTileServer(t *testing.T, zoom int) *tileServer {
	ts := &tileServer{requests: map[coord]int{}}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var z, x, y int
		if _, err := fmt.Sscanf(r.URL.Path, "/%d/%d/%d.png", &z, &x, &y); err != nil || z != zoom {
			http.NotFound(w, r)
			return
		}
		ts.mtx.Lock()
		ts.requests[coord{x, y}]++
		ts.total++
		n, cb := ts.total, ts.onReq
		ts.mtx.Unlock()
		if cb != nil {
			cb(n)
		}
		w.Write(tileBody(x, y))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func tileBody(x, y int) []byte {
	return fmt.Appendf(nil, "tile %d/%d", x, y)
}

func (ts *tileServer) fetcher(t *testing.T, zoom int) *fetcher {
	ep, err := parseEndpoint(ts.URL, zoom)
	if err != nil {
		t.Fatal(err)
	}
	return newFetcher(ep, 4, 10*time.Second, 2, "test", false)
}

func TestResumeAfterInterrupt(t *testing.T) {
	const zoom = 5
	dim := 1 << zoom
	total := uint64(dim * dim)
	fname := filepath.Join(t.TempDir(), fmt.Sprintf("%d.tiles", zoom))
	ts := newTileServer(t, zoom)
	ftch := ts.fetcher(t, zoom)

	if n, err := existingTiles(fname, zoom); err != nil || n != 0 {
		t.Fatalf("fresh output: %d, %v", n, err)
	}

	// first run, interrupted part way through
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ts.onReq = func(n int) {
		if n == int(total)/3 {
			cancel()
		}
	}
	tm, err := tilemap.NewTilemap(fname, zoom, false)
	if err != nil {
		t.Fatal(err)
	}
	var st stats
	if err = run(ctx, tm, ftch, dim, 4, true, &st); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected an interrupted run, got %v", err)
	} else if err = tm.Close(); err != nil {
		t.Fatal(err)
	}

	present, err := existingTiles(fname, zoom)
	if err != nil {
		t.Fatal(err)
	} else if present == 0 || present >= total {
		t.Fatalf("expected a partial map, have %d/%d", present, total)
	} else if present != st.fetched.Load() {
		t.Fatalf("scan found %d tiles, first run stored %d", present, st.fetched.Load())
	}

	// remember what the first run stored, none of it may be fetched again
	tm, err = tilemap.NewTilemap(fname, zoom, false)
	if err != nil {
		t.Fatal(err)
	}
	stored := map[coord]bool{}
	for x := range dim {
		for y := range dim {
			if ok, err := tm.Has(x, y); err != nil {
				t.Fatal(err)
			} else if ok {
				stored[coord{x, y}] = true
			}
		}
	}
	ts.mtx.Lock()
	ts.onReq = nil
	before := map[coord]int{}
	for c, n := range ts.requests {
		before[c] = n
	}
	ts.mtx.Unlock()

	// second run resumes and completes
	st = stats{}
	st.skipped.Store(present)
	if err = run(context.Background(), tm, ftch, dim, 4, true, &st); err != nil {
		t.Fatal(err)
	} else if err = tm.Close(); err != nil {
		t.Fatal(err)
	} else if st.fetched.Load() != total-present {
		t.Fatalf("resume fetched %d tiles, expected %d", st.fetched.Load(), total-present)
	} else if st.done() != total {
		t.Fatalf("resume accounted for %d/%d tiles", st.done(), total)
	}
	ts.mtx.Lock()
	for c := range stored {
		if ts.requests[c] != before[c] {
			t.Errorf("tile %d/%d was already stored but requested again", c.x, c.y)
		}
	}
	ts.mtx.Unlock()

	if present, err = existingTiles(fname, zoom); err != nil || present != total {
		t.Fatalf("expected a complete map, have %d/%d: %v", present, total, err)
	}
	rdr, err := tilemap.NewTilemap(fname, zoom, true)
	if err != nil {
		t.Fatal(err)
	}
	defer rdr.Close()
	for x := range dim {
		for y := range dim {
			if b, err := rdr.GetTile(x, y); err != nil {
				t.Fatal(err)
			} else if string(b) != string(tileBody(x, y)) {
				t.Fatalf("tile %d/%d holds %q", x, y, b)
			}
		}
	}
}

func TestExistingTilesZoomMismatch(t *testing.T) {
	dir := t.TempDir()
	// a partial zoom 4 map renamed so it looks like zoom 3 and zoom 5
	src := filepath.Join(dir, "src.tiles")
	tm, err := tilemap.NewTilemap(src, 4, false)
	if err != nil {
		t.Fatal(err)
	}
	for y := range 16 {
		if err = tm.Add(0, y, tileBody(0, y)); err != nil {
			t.Fatal(err)
		}
	}
	if err = tm.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := existingTiles(src, 4); err != nil || n != 16 {
		t.Fatalf("right zoom: %d, %v", n, err)
	}
	for _, zoom := range []int{3, 5} {
		if _, err = existingTiles(src, zoom); err == nil {
			t.Fatalf("zoom 4 map accepted as zoom %d", zoom)
		}
	}

	// an empty file is a fresh start
	empty := filepath.Join(dir, "empty.tiles")
	if err = os.WriteFile(empty, nil, 0640); err != nil {
		t.Fatal(err)
	} else if n, err := existingTiles(empty, 4); err != nil || n != 0 {
		t.Fatalf("empty file: %d, %v", n, err)
	}
	if _, err = existingTiles(dir, 4); err == nil {
		t.Fatal("directory accepted as a tilemap")
	}
}
