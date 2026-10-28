package tilemap

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/dchest/siphash"
)

var (
	tdir      string
	basicBuff []byte
	tpaths    = map[testing.TB]bool{}
)

func TestMain(m *testing.M) {
	var err error
	var r int

	basicBuff = make([]byte, 1024*64)
	rand.Read(basicBuff)
	if tdir, err = os.MkdirTemp(os.TempDir(), `tilemap`); err != nil {
		fmt.Printf("Failed to create tempdir: %v", err)
		r = -1
	} else {
		r = m.Run()
	}
	os.RemoveAll(tdir)
	os.Exit(r)
}

// tpath hands out a unique file for each test so zoom levels never collide.
// The file is removed on the first call in each test so -count > 1 starts
// fresh.
func tpath(t testing.TB) string {
	p := filepath.Join(tdir, t.Name()+`.tiles`)
	if !tpaths[t] {
		tpaths[t] = true
		os.Remove(p)
	}
	return p
}

func TestEncodeDecode(t *testing.T) {
	var dp datapointer
	//test with bad buffer
	buff := make([]byte, 4)
	if err := dp.Decode(buff); err == nil {
		t.Fatal("Failed to catch small buffer")
	} else if err = dp.Decode(nil); err == nil {
		t.Fatal("Failed to catch nil buffer")
	}
	buff = make([]byte, 10)
	if err := dp.Decode(buff); err != nil {
		t.Fatal(err)
	} else if dp.size != 0 || dp.offset != 0 {
		t.Fatal("bad decode")
	}

	dp.offset = 0xffdeadbeef
	dp.size = 0x12345678
	if err := dp.Encode(buff); err != nil {
		t.Fatal(err)
	}

	var dp2 datapointer
	if err := dp2.Decode(buff); err != nil {
		t.Fatal(err)
	}
	if dp != dp2 {
		t.Fatalf("Ivalid decode values: %+v != %+v", dp, dp2)
	}
}

func TestTileID(t *testing.T) {
	if tileid(0, 0, 0) != 0 {
		t.Fatal("zoom 0 must map to tile 0")
	}
	for zoom := 1; zoom <= 4; zoom++ {
		dim := 1 << zoom
		seen := make(map[uint32]bool, dim*dim)
		for x := range dim {
			for y := range dim {
				id := tileid(zoom, x, y)
				if want := uint32(x*dim + y); id != want {
					t.Fatalf("zoom %d %d/%d: %d != %d", zoom, x, y, id, want)
				} else if seen[id] {
					t.Fatalf("zoom %d %d/%d: duplicate id %d", zoom, x, y, id)
				}
				seen[id] = true
			}
		}
	}
	// make sure the largest zoom level does not overflow
	if id := tileid(MaxZoom, (1<<MaxZoom)-1, (1<<MaxZoom)-1); id != 0xffffffff {
		t.Fatalf("max tile id %x", id)
	}
	if tileCount(0) != 1 || tileCount(1) != 4 || tileCount(16) != 1<<32 {
		t.Fatal("bad tile count")
	}
}

func TestNewTilemap(t *testing.T) {
	if _, err := NewTilemap(tpath(t), MaxZoom+1, false); !errors.Is(err, ErrInvalidDimension) {
		t.Fatalf("failed to catch bad zoom: %v", err)
	} else if _, err = NewTilemap(``, 1, false); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("failed to catch bad path: %v", err)
	} else if _, err = NewTilemap(filepath.Join(tdir, `missing`), 1, true); err == nil {
		t.Fatal("failed to catch missing read only file")
	}
	wtr, err := NewTilemap(tpath(t), 2, false)
	if err != nil {
		t.Fatal(err)
	}
	//check some tids
	if tid := wtr.tileid(0, 2); tid != (0*4 + 2) {
		t.Fatalf("invalid tile id %d", tid)
	} else if tid = wtr.tileid(1, 0); tid != (1*4 + 0) {
		t.Fatalf("invalid tile id %d", tid)
	} else if tid = wtr.tileid(2, 2); tid != (2*4 + 2) {
		t.Fatalf("invalid tile id %d", tid)
	} else if tid = wtr.tileid(3, 1); tid != (3*4 + 1) {
		t.Fatalf("invalid tile id %d", tid)
	}
	if wtr.Size() != tileCount(2)*dpsize {
		t.Fatalf("bad initial size %d", wtr.Size())
	}
	if err = wtr.Close(); err != nil {
		t.Fatal(err)
	} else if err = wtr.Close(); err != nil {
		t.Fatalf("second close must be a no-op: %v", err)
	}
	if _, err = wtr.GetTile(0, 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("expected ErrClosed, got %v", err)
	} else if err = wtr.Add(0, 0, basicBuff[:10]); !errors.Is(err, ErrClosed) {
		t.Fatalf("expected ErrClosed, got %v", err)
	}
	// a file that is too small for the zoom level must be rejected read only
	if _, err = NewTilemap(tpath(t), 3, true); !errors.Is(err, ErrInvalidDPRegionSize) {
		t.Fatalf("failed to catch undersized file: %v", err)
	}
}

func TestTilemap(t *testing.T) {
	zl := 4
	wtr, err := NewTilemap(tpath(t), zl, false)
	if err != nil {
		t.Fatal(err)
	}
	dim := 1 << zl
	for i := range dim {
		for j := range dim {
			if err = wtr.Add(i, j, randBuff(basicBuff)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err = wtr.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTilemapBadAdd(t *testing.T) {
	wtr, err := NewTilemap(tpath(t), 2, false)
	if err != nil {
		t.Fatal(err)
	}
	defer wtr.Close()
	if err = wtr.Add(4, 0, basicBuff[:10]); !errors.Is(err, ErrInvalidTileID) {
		t.Fatalf("failed to catch bad x: %v", err)
	} else if err = wtr.Add(0, -1, basicBuff[:10]); !errors.Is(err, ErrInvalidTileID) {
		t.Fatalf("failed to catch bad y: %v", err)
	} else if err = wtr.Add(0, 0, nil); !errors.Is(err, ErrInvalidTileBuffer) {
		t.Fatalf("failed to catch empty buffer: %v", err)
	}
	if _, err = wtr.GetTile(4, 0); !errors.Is(err, ErrInvalidTileID) {
		t.Fatalf("failed to catch bad x on get: %v", err)
	} else if _, err = wtr.GetTile(1, 1); !errors.Is(err, ErrTileNotFound) {
		t.Fatalf("expected ErrTileNotFound, got %v", err)
	}
	if ok, err := wtr.Has(1, 1); err != nil || ok {
		t.Fatalf("Has on a missing tile: %v %v", ok, err)
	}
}

func TestTilemapHighCollision(t *testing.T) {
	zl := 8
	wtr, err := NewTilemap(tpath(t), zl, false)
	if err != nil {
		t.Fatal(err)
	}
	buff := basicBuff[0 : 8*1024]
	for i := range zl {
		for j := range zl {
			if err = wtr.Add(i, j, randBuff(buff)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err = wtr.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTilemapDedup(t *testing.T) {
	wtr, err := NewTilemap(tpath(t), 3, false)
	if err != nil {
		t.Fatal(err)
	}
	tile := basicBuff[:4096]
	base := wtr.Size()
	for i := range 8 {
		for j := range 8 {
			if err = wtr.Add(i, j, tile); err != nil {
				t.Fatal(err)
			}
		}
	}
	// 64 identical tiles must only be stored once
	if sz := wtr.Size(); sz != base+int64(len(tile)) {
		t.Fatalf("dedup failed, size grew to %d, expected %d", sz, base+int64(len(tile)))
	}
	dp0, err := wtr.getDataPointer(wtr.tileid(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	dp7, err := wtr.getDataPointer(wtr.tileid(7, 7))
	if err != nil {
		t.Fatal(err)
	} else if dp0 != dp7 {
		t.Fatalf("tiles not aliased: %+v != %+v", dp0, dp7)
	}
	// simulate a hash collision by pointing the hash at a tile with other content
	other := basicBuff[8192 : 8192+4096]
	if err = wtr.Add(0, 1, other); err != nil {
		t.Fatal(err)
	}
	collided := basicBuff[16384 : 16384+4096]
	key := siphash.Hash(sipkey1, sipkey2, collided)
	wtr.hmp[key] = wtr.tileid(0, 1)
	if err = wtr.Add(1, 0, collided); err != nil {
		t.Fatal(err)
	}
	if got, err := wtr.GetTile(1, 0); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(got, collided) {
		t.Fatal("hash collision aliased the wrong tile")
	}
	if got, err := wtr.GetTile(0, 1); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(got, other) {
		t.Fatal("collision handling clobbered the existing tile")
	}
	if err = wtr.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTilemapReadWrite(t *testing.T) {
	testMap := make(map[uint32]uint64, 1024)
	zl := 2
	wtr, err := NewTilemap(tpath(t), zl, false)
	if err != nil {
		t.Fatal(err)
	}
	dim := 1 << zl
	for i := range dim {
		for j := range dim {
			buff := randBuff(basicBuff)
			key := siphash.Hash(sipkey1, sipkey2, buff)
			if err = wtr.Add(i, j, buff); err != nil {
				t.Fatal(err)
			}
			testMap[tileid(zl, i, j)] = key
		}
	}

	// reads on a writer must see tiles that are still sitting in the buffer
	for i := range dim {
		for j := range dim {
			if ok, err := wtr.Has(i, j); err != nil || !ok {
				t.Fatalf("Has %d/%d: %v %v", i, j, ok, err)
			}
			if buff, err := wtr.GetTile(i, j); err != nil {
				t.Fatal(err)
			} else {
				key := siphash.Hash(sipkey1, sipkey2, buff)
				if h, ok := testMap[tileid(zl, i, j)]; !ok {
					t.Fatal("could not file test hash key")
				} else if h != key {
					t.Fatalf("Invalid result hash on %d %d: %v != %v", i, j, h, key)
				}
			}
		}
	}
	if err = wtr.Sync(); err != nil {
		t.Fatal(err)
	}
	if err = wtr.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTilemapReadWriteReopen(t *testing.T) {
	testMap := make(map[uint32]uint64, 1024)
	zl := 4
	dim := 1 << zl
	wtr, err := NewTilemap(tpath(t), zl, false)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < dim/2; i++ {
		for j := range dim {
			buff := randBuff(basicBuff)
			key := siphash.Hash(sipkey1, sipkey2, buff)
			if err = wtr.Add(i, j, buff); err != nil {
				t.Fatal(err)
			}
			testMap[tileid(zl, i, j)] = key
		}
	}
	if err = wtr.Close(); err != nil {
		t.Fatal(err)
	}
	if wtr, err = NewTilemap(tpath(t), zl, false); err != nil {
		t.Fatal(err)
	}
	// resume support, the first half must be visible after reopen
	for i := range dim {
		for j := range dim {
			if ok, err := wtr.Has(i, j); err != nil {
				t.Fatal(err)
			} else if ok != (i < dim/2) {
				t.Fatalf("Has %d/%d = %v", i, j, ok)
			}
		}
	}
	for i := dim / 2; i < dim; i++ {
		for j := range dim {
			buff := randBuff(basicBuff)
			key := siphash.Hash(sipkey1, sipkey2, buff)
			if err = wtr.Add(i, j, buff); err != nil {
				t.Fatal(err)
			}
			testMap[tileid(zl, i, j)] = key
		}
	}
	if err = wtr.Close(); err != nil {
		t.Fatal(err)
	}

	rdr, err := NewTilemap(tpath(t), zl, true)
	if err != nil {
		t.Fatal(err)
	}
	for i := range dim {
		for j := range dim {
			if buff, err := rdr.GetTile(i, j); err != nil {
				t.Fatal(err)
			} else {
				key := siphash.Hash(sipkey1, sipkey2, buff)
				if h, ok := testMap[tileid(zl, i, j)]; !ok {
					t.Fatal("could not file test hash key")
				} else if h != key {
					t.Fatalf("Invalid result hash on %d %d: %v != %v", i, j, h, key)
				}
			}
		}
	}

	//check that we can't write to it
	if err = rdr.Add(0, 0, randBuff(basicBuff)); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Failed to catch write on read only: %v", err)
	}
	if err = rdr.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTilemapReadOnlyMissing(t *testing.T) {
	wtr, err := NewTilemap(tpath(t), 2, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = wtr.Add(1, 1, basicBuff[:100]); err != nil {
		t.Fatal(err)
	} else if err = wtr.Close(); err != nil {
		t.Fatal(err)
	}
	rdr, err := NewTilemap(tpath(t), 2, true)
	if err != nil {
		t.Fatal(err)
	}
	defer rdr.Close()
	if _, err = rdr.GetTile(0, 0); !errors.Is(err, ErrTileNotFound) {
		t.Fatalf("expected ErrTileNotFound, got %v", err)
	} else if buff, err := rdr.GetTile(1, 1); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(buff, basicBuff[:100]) {
		t.Fatal("bad tile contents")
	}
}

// TestTilemapUnflushedIndex simulates a writer that is killed before it can
// flush.  Another handle on the same file sees exactly what the disk would
// hold after the kill, and must never find an index entry that points at
// bytes that were never written.
func TestTilemapUnflushedIndex(t *testing.T) {
	zl := 6
	dim := 1 << zl
	wtr, err := NewTilemap(tpath(t), zl, false)
	if err != nil {
		t.Fatal(err)
	}
	defer wtr.Close()

	// a handful of small tiles stays entirely in the append buffer
	if err = wtr.Add(0, 0, basicBuff[:100]); err != nil {
		t.Fatal(err)
	} else if err = wtr.Add(0, 2, basicBuff[100:300]); err != nil {
		t.Fatal(err)
	}
	checkDiskIndex(t, zl, 0)

	// verifying a duplicate reads the original back from the file, which
	// flushes and commits both buffered tiles along with the alias
	if err = wtr.Add(0, 1, basicBuff[:100]); err != nil {
		t.Fatal(err)
	}
	checkDiskIndex(t, zl, 3)

	// fill well past the buffer so it flushes on its own several times, the
	// tail of the run is still buffered when we look
	var added int
	for i := 1; i < dim; i++ {
		for j := range dim {
			if err = wtr.Add(i, j, randBuff(basicBuff)); err != nil {
				t.Fatal(err)
			}
			added++
		}
	}
	if found := checkDiskIndex(t, zl, -1); found == 0 || found >= added {
		t.Fatalf("expected a partial flush, found %d of %d tiles", found, added)
	}

	// an explicit flush makes everything visible
	if err = wtr.Flush(); err != nil {
		t.Fatal(err)
	}
	checkDiskIndex(t, zl, 3+added)
}

// checkDiskIndex opens a second handle on the test file and makes sure every
// index entry is readable.  It returns the number of tiles present, and fails
// if want is not negative and does not match.
func checkDiskIndex(t *testing.T, zl, want int) (found int) {
	t.Helper()
	rdr, err := NewTilemap(tpath(t), zl, true)
	if err != nil {
		t.Fatal(err)
	}
	defer rdr.Close()
	dim := 1 << zl
	for i := range dim {
		for j := range dim {
			if _, err := rdr.GetTile(i, j); err == nil {
				found++
			} else if !errors.Is(err, ErrTileNotFound) {
				t.Fatalf("tile %d/%d: %v", i, j, err)
			}
		}
	}
	if want >= 0 && found != want {
		t.Fatalf("found %d tiles on disk, expected %d", found, want)
	}
	return
}

// TestTilemapConcurrent hammers a single read/write handle from several
// writers and readers at once.  It is only meaningful under go test -race.
func TestTilemapConcurrent(t *testing.T) {
	zl := 5
	dim := 1 << zl
	wtr, err := NewTilemap(tpath(t), zl, false)
	if err != nil {
		t.Fatal(err)
	}
	const writers = 4
	const readers = 4

	var wg sync.WaitGroup
	errs := make(chan error, writers+readers+1)
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			// each writer owns a band of columns, and every other tile is
			// a duplicate so the dedup path is exercised too
			for i := w; i < dim; i += writers {
				for j := range dim {
					buff := randBuff(basicBuff)
					if j%2 == 1 {
						buff = basicBuff[:512]
					}
					if err := wtr.Add(i, j, buff); err != nil {
						errs <- err
						return
					}
				}
				if err := wtr.Flush(); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	stop := make(chan struct{})
	var rwg sync.WaitGroup
	for r := range readers {
		rwg.Add(1)
		go func(r int) {
			defer rwg.Done()
			for n := r; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				x, y := n%dim, (n/dim)%dim
				ok, err := wtr.Has(x, y)
				if err != nil {
					errs <- err
					return
				} else if !ok {
					continue
				}
				buff, err := wtr.GetTile(x, y)
				if err != nil {
					errs <- err
					return
				} else if y%2 == 1 && !bytes.Equal(buff, basicBuff[:512]) {
					errs <- fmt.Errorf("bad duplicate tile at %d/%d", x, y)
					return
				}
				_ = wtr.Size()
			}
		}(r)
	}
	wg.Wait()
	if err = wtr.Sync(); err != nil {
		t.Fatal(err)
	}
	close(stop)
	rwg.Wait()

	// Close racing with readers must be clean, they see ErrClosed or a tile
	rwg.Go(func() {
		for i := 0; i < dim*dim; i++ {
			if _, err := wtr.GetTile(i%dim, i/dim); err != nil && !errors.Is(err, ErrClosed) {
				errs <- err
				return
			}
		}
	})
	if err = wtr.Close(); err != nil {
		t.Fatal(err)
	}
	rwg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if err = wtr.Add(0, 0, basicBuff[:10]); !errors.Is(err, ErrClosed) {
		t.Fatalf("expected ErrClosed after close, got %v", err)
	}
	checkDiskIndex(t, zl, dim*dim)
}

func BenchmarkAddUnique(b *testing.B) {
	wtr, err := NewTilemap(tpath(b), 12, false)
	if err != nil {
		b.Fatal(err)
	}
	defer wtr.Close()
	// every tile differs in its first bytes so none dedup
	tile := make([]byte, 4096)
	copy(tile, basicBuff)
	b.ResetTimer()
	b.SetBytes(int64(len(tile)))
	for i := 0; i < b.N; i++ {
		binary.LittleEndian.PutUint32(tile, uint32(i))
		if err = wtr.Add(i&4095, (i>>12)&4095, tile); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAddDuplicate(b *testing.B) {
	wtr, err := NewTilemap(tpath(b), 10, false)
	if err != nil {
		b.Fatal(err)
	}
	defer wtr.Close()
	tile := basicBuff[:4096]
	b.ResetTimer()
	b.SetBytes(int64(len(tile)))
	for i := 0; i < b.N; i++ {
		if err = wtr.Add(i&1023, (i>>10)&1023, tile); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGetTile(b *testing.B) {
	wtr, err := NewTilemap(tpath(b), 6, false)
	if err != nil {
		b.Fatal(err)
	}
	for i := range 64 {
		for j := range 64 {
			if err = wtr.Add(i, j, randBuff(basicBuff)); err != nil {
				b.Fatal(err)
			}
		}
	}
	if err = wtr.Close(); err != nil {
		b.Fatal(err)
	}
	rdr, err := NewTilemap(tpath(b), 6, true)
	if err != nil {
		b.Fatal(err)
	}
	defer rdr.Close()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			if _, err := rdr.GetTile(i&63, (i>>6)&63); err != nil {
				b.Fatal(err)
			}
			i++
		}
	})
}

func randBuff(b []byte) []byte {
	l := rand.Intn(8*1024) + 1
	if l >= len(b) {
		l = len(b) - 2
	}
	off := rand.Intn(len(b) - l)
	return b[off : off+l]
}
