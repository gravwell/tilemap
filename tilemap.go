// Package tilemap implements a compact on-disk store for a single zoom level
// of map tiles.
//
// A tilemap file is laid out as a fixed size index region followed by a
// densely packed data region:
//
//	[ datapointer * 4^zoom ][ tile bytes ... ]
//
// Each datapointer is 10 bytes: a 48 bit absolute file offset and a 32 bit
// size.  The index region is memory mapped so that looking up a tile is a
// single bounds check and a 10 byte decode.  Identical tiles (think ocean) are
// stored once and referenced by every tile that shares the bytes.
package tilemap

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"

	"github.com/dchest/siphash"
	"github.com/tysonmote/gommap"
)

const (
	dpsize       = 6 + 4 // 48bit offset, 4 byte size
	maxDimension = 16

	// writeBufferSize is the size of the append buffer used in read/write
	// mode.  Tiles are typically a few KB so this coalesces hundreds of
	// tiles into a single write syscall.
	writeBufferSize = 1024 * 1024

	// MaxZoom is the largest zoom level a tilemap can hold.
	MaxZoom = maxDimension

	// MaxTileSize is the largest single tile that can be stored.
	MaxTileSize = 1 << 24

	wtrMapFlags  = gommap.PROT_WRITE | gommap.PROT_READ
	wtrOpenFlags = os.O_RDWR | os.O_CREATE
	rdrMapFlags  = gommap.PROT_READ
	rdrOpenFlags = os.O_RDONLY
)

var (
	ErrInvalidBufferSize   = errors.New("invalid buffer size")
	ErrInvalidDimension    = errors.New("invalid tile zoom")
	ErrInvalidTileID       = errors.New("invalid tile id")
	ErrInvalidTileBuffer   = errors.New("invalid tile buffer")
	ErrInvalidDatapointer  = errors.New("invalid tile datapointer, file may be corrupt")
	ErrInvalidDPRegionSize = errors.New("invalid tile datapointer region size, file may be corrupt")
	ErrPartialWrite        = errors.New("partial write")
	ErrReadOnly            = errors.New("tilemap is read only")
	ErrClosed              = errors.New("tilemap is closed")
	ErrTileNotFound        = errors.New("tile not found")
	ErrInvalidPath         = errors.New("invalid path")
)

// Tilemap is a single zoom level tile store.  It is safe for concurrent use.
type Tilemap struct {
	mtx  sync.RWMutex
	ro   bool
	zoom int
	dim  int // tiles per side, 1<<zoom
	hmp  map[uint64]uint32
	fio  *os.File
	mm   gommap.MMap // datapointer index region (always mapped)
	data gommap.MMap // whole file mapping, read only mode only
	wtr  *bufio.Writer
	// pend holds index entries whose tile bytes may still be sitting in wtr.
	// They are only written to the mapped index once the bytes have reached
	// the file, so a killed process can never leave an index entry pointing
	// at data that was never written.
	pend map[uint32]datapointer
	cmp  []byte // scratch buffer for dedup verification
	foff int64  // logical end of file, including buffered but unflushed bytes
	open bool
}

// NewTilemap opens (or in read/write mode creates) the tilemap at pth for the
// given zoom level.  When ro is true the file is opened read only and the
// Add method returns ErrReadOnly.
func NewTilemap(pth string, zoom int, ro bool) (w *Tilemap, err error) {
	if zoom < 0 || zoom > maxDimension {
		return nil, ErrInvalidDimension
	} else if pth == `` {
		return nil, ErrInvalidPath
	}
	oflags, mapflags := wtrOpenFlags, gommap.ProtFlags(wtrMapFlags)
	if ro {
		oflags, mapflags = rdrOpenFlags, rdrMapFlags
	}

	fio, err := os.OpenFile(pth, oflags, 0640)
	if err != nil {
		return nil, err
	}
	// everything below here must close fio on failure
	defer func() {
		if err != nil {
			fio.Close()
		}
	}()

	dpRegionSize := int64(tileCount(zoom)) * dpsize
	var fsize int64
	if ro {
		fsize, err = checkFileMap(fio, zoom)
	} else {
		// best effort, may not be supported by the filesystem
		setNoCOW(fio)
		fsize, err = prepFileMap(fio, zoom)
	}
	if err != nil {
		return nil, err
	} else if fsize < dpRegionSize {
		return nil, ErrInvalidDPRegionSize
	}

	mm, err := gommap.MapRegion(fio.Fd(), 0, dpRegionSize, mapflags, gommap.MAP_SHARED)
	if err != nil {
		return nil, errorLine(err)
	}
	w = &Tilemap{
		ro:   ro,
		zoom: zoom,
		dim:  1 << uint(zoom),
		fio:  fio,
		mm:   mm,
		foff: fsize,
		open: true,
	}
	if ro {
		// map the entire file so reads are a bounds check and a copy
		if w.data, err = gommap.MapRegion(fio.Fd(), 0, fsize, rdrMapFlags, gommap.MAP_SHARED); err != nil {
			mm.UnsafeUnmap()
			return nil, errorLine(err)
		}
	} else {
		// all appends go through a buffered writer positioned at the end
		if _, err = fio.Seek(fsize, io.SeekStart); err != nil {
			mm.UnsafeUnmap()
			return nil, errorLine(err)
		}
		w.wtr = bufio.NewWriterSize(fio, writeBufferSize)
		w.hmp = make(map[uint64]uint32, 1024)
		w.pend = make(map[uint32]datapointer, 1024)
	}
	return w, nil
}

// Zoom returns the zoom level held by the tilemap.
func (w *Tilemap) Zoom() int {
	return w.zoom
}

// Size returns the current logical size of the tilemap file in bytes.
func (w *Tilemap) Size() int64 {
	w.mtx.RLock()
	defer w.mtx.RUnlock()
	return w.foff
}

// Add stores buff as the tile at x, y.  If an identical tile has already been
// added through this handle the bytes are shared rather than written again.
func (w *Tilemap) Add(x, y int, buff []byte) (err error) {
	if w.ro {
		return ErrReadOnly
	} else if x < 0 || x >= w.dim || y < 0 || y >= w.dim {
		return fmt.Errorf("%w %d/%d", ErrInvalidTileID, x, y)
	} else if len(buff) == 0 || len(buff) > MaxTileSize {
		return fmt.Errorf("%w (%d bytes)", ErrInvalidTileBuffer, len(buff))
	}
	w.mtx.Lock()
	defer w.mtx.Unlock()
	if !w.open {
		return ErrClosed
	}
	tid := w.tileid(x, y)
	key := siphash.Hash(sipkey1, sipkey2, buff)
	if extid, ok := w.hmp[key]; ok {
		// potential duplicate, verify the bytes actually match before
		// aliasing the tile so a hash collision can never serve the wrong tile
		var dp datapointer
		var same bool
		if dp, err = w.getDataPointer(extid); err != nil {
			return errorLine(err)
		} else if same, err = w.sameContents(dp, buff); err != nil {
			return errorLine(err)
		} else if same {
			return w.setDataPointer(tid, dp)
		}
	}
	// flush ourselves rather than letting bufio do it mid write, so pending
	// index entries are committed in step with the data and pend stays small
	if w.wtr.Available() < len(buff) {
		if err = w.flush(); err != nil {
			return errorLine(err)
		}
	}
	dp, err := w.writeNewBuffer(buff)
	if err != nil {
		return errorLine(err)
	}
	if _, ok := w.hmp[key]; !ok {
		w.hmp[key] = tid
	}
	if err = w.setDataPointer(tid, dp); err != nil {
		err = fmt.Errorf("failed to set datapointer for %d/%d: %w", x, y, err)
	}
	return
}

// sameContents reports whether the stored tile at dp holds exactly buff.
// The caller must hold the write lock.
func (w *Tilemap) sameContents(dp datapointer, buff []byte) (bool, error) {
	if dp.size != int64(len(buff)) {
		return false, nil
	} else if err := w.flushFor(dp); err != nil {
		return false, err
	}
	if cap(w.cmp) < len(buff) {
		w.cmp = make([]byte, len(buff))
	}
	cmp := w.cmp[:len(buff)]
	if _, err := w.fio.ReadAt(cmp, dp.offset); err != nil {
		return false, err
	}
	return bytes.Equal(cmp, buff), nil
}

// flushFor makes sure the bytes referenced by dp are on disk rather than
// sitting in the append buffer.  The caller must hold the write lock.
func (w *Tilemap) flushFor(dp datapointer) error {
	if w.isFlushed(dp) {
		return nil
	}
	return w.flush()
}

// isFlushed reports whether the bytes referenced by dp have been handed to
// the file.  The caller must hold the write lock.
func (w *Tilemap) isFlushed(dp datapointer) bool {
	if w.wtr == nil {
		return true
	}
	return dp.offset+dp.size <= w.foff-int64(w.wtr.Buffered())
}

// flush writes out the append buffer and then commits the pending index
// entries.  The caller must hold the write lock.
func (w *Tilemap) flush() error {
	if err := w.wtr.Flush(); err != nil {
		return err
	}
	return w.commitPending()
}

// commitPending writes the pending index entries into the mapped index.  The
// caller must hold the write lock and must have flushed the append buffer.
func (w *Tilemap) commitPending() error {
	for tid, dp := range w.pend {
		if err := w.encodeDataPointer(tid, dp); err != nil {
			return err
		}
		delete(w.pend, tid)
	}
	return nil
}

func (w *Tilemap) writeNewBuffer(buff []byte) (dp datapointer, err error) {
	var n int
	if n, err = w.wtr.Write(buff); err != nil {
		return
	} else if n != len(buff) {
		err = ErrPartialWrite
		return
	}
	dp.offset = w.foff
	dp.size = int64(len(buff))
	w.foff += dp.size
	return
}

func (w *Tilemap) getDataPointer(tid uint32) (dp datapointer, err error) {
	if pdp, ok := w.pend[tid]; ok {
		return pdp, nil
	}
	toff := int64(tid) * dpsize
	if toff+dpsize > int64(len(w.mm)) {
		err = fmt.Errorf("%w %d: %d > %d", ErrInvalidTileID, tid, toff+dpsize, len(w.mm))
	} else {
		err = dp.Decode(w.mm[toff : toff+dpsize])
	}
	return
}

// setDataPointer points tid at dp.  If the bytes for dp are still buffered
// the entry is held in pend until they are flushed.  The caller must hold the
// write lock.
func (w *Tilemap) setDataPointer(tid uint32, dp datapointer) (err error) {
	if !w.isFlushed(dp) {
		w.pend[tid] = dp
		return nil
	}
	// the new entry supersedes any older one still waiting on a flush
	delete(w.pend, tid)
	return w.encodeDataPointer(tid, dp)
}

func (w *Tilemap) encodeDataPointer(tid uint32, dp datapointer) (err error) {
	toff := int64(tid) * dpsize
	if toff+dpsize > int64(len(w.mm)) {
		err = fmt.Errorf("%w %d: %d > %d", ErrInvalidTileID, tid, toff+dpsize, len(w.mm))
	} else {
		err = dp.Encode(w.mm[toff : toff+dpsize])
	}
	return
}

// lookup validates x, y and returns the datapointer for the tile.  It returns
// ErrTileNotFound for a tile that has never been added.  The caller must hold
// at least the read lock.
func (w *Tilemap) lookup(x, y int) (dp datapointer, err error) {
	if !w.open {
		return dp, ErrClosed
	} else if x < 0 || x >= w.dim || y < 0 || y >= w.dim {
		return dp, fmt.Errorf("%w %d/%d", ErrInvalidTileID, x, y)
	}
	if dp, err = w.getDataPointer(w.tileid(x, y)); err != nil {
		return
	} else if dp.size == 0 {
		return dp, ErrTileNotFound
	}
	// the data region starts after the index, and must end within the file
	if dp.offset < int64(len(w.mm)) || dp.offset+dp.size > w.foff {
		err = fmt.Errorf("%w %x:%x not within %x:%x", ErrInvalidDatapointer,
			dp.offset, dp.offset+dp.size, len(w.mm), w.foff)
	}
	return
}

// Has reports whether a tile has been stored at x, y.
func (w *Tilemap) Has(x, y int) (bool, error) {
	w.mtx.RLock()
	defer w.mtx.RUnlock()
	if _, err := w.lookup(x, y); err != nil {
		if errors.Is(err, ErrTileNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// GetTile returns a copy of the tile stored at x, y.  ErrTileNotFound is
// returned for a tile that was never added.
func (w *Tilemap) GetTile(x, y int) (buff []byte, err error) {
	if w.ro {
		w.mtx.RLock()
		defer w.mtx.RUnlock()
		var dp datapointer
		if dp, err = w.lookup(x, y); err != nil {
			return nil, err
		}
		buff = make([]byte, dp.size)
		copy(buff, w.data[dp.offset:dp.offset+dp.size])
		return
	}
	// read/write mode may need to flush the append buffer, so take the
	// exclusive lock
	w.mtx.Lock()
	defer w.mtx.Unlock()
	var dp datapointer
	if dp, err = w.lookup(x, y); err != nil {
		return nil, err
	} else if err = w.flushFor(dp); err != nil {
		return nil, errorLine(err)
	}
	buff = make([]byte, dp.size)
	if _, err = w.fio.ReadAt(buff, dp.offset); err != nil {
		return nil, errorLine(err)
	}
	return
}

// Flush pushes any buffered tile data out to the operating system.
func (w *Tilemap) Flush() error {
	if w.ro {
		return nil
	}
	w.mtx.Lock()
	defer w.mtx.Unlock()
	if !w.open {
		return ErrClosed
	}
	return w.flush()
}

// Sync flushes buffered tile data and forces both the index and the data to
// stable storage.  The data is synced before the new index entries are
// written so the index can never reach the disk ahead of the tiles it
// references.
func (w *Tilemap) Sync() error {
	if w.ro {
		return nil
	}
	w.mtx.Lock()
	defer w.mtx.Unlock()
	if !w.open {
		return ErrClosed
	}
	if err := w.wtr.Flush(); err != nil {
		return errorLine(err)
	} else if err = w.fio.Sync(); err != nil {
		return errorLine(err)
	} else if err = w.commitPending(); err != nil {
		return errorLine(err)
	} else if err = w.mm.Sync(gommap.MS_SYNC); err != nil {
		return errorLine(err)
	}
	return nil
}

// Close flushes outstanding writes and releases the file and mappings.
// Closing an already closed tilemap is a no-op.
func (w *Tilemap) Close() (err error) {
	w.mtx.Lock()
	defer w.mtx.Unlock()
	if !w.open {
		return nil
	}
	w.open = false
	if w.wtr != nil {
		if err = w.flush(); err != nil {
			err = errorLine(err)
		}
		w.wtr = nil
	}
	w.hmp = nil
	w.pend = nil
	w.cmp = nil
	if w.data != nil {
		if lerr := w.data.UnsafeUnmap(); lerr != nil && err == nil {
			err = errorLine(lerr)
		}
		w.data = nil
	}
	if lerr := w.mm.UnsafeUnmap(); lerr != nil && err == nil {
		err = errorLine(lerr)
	}
	w.mm = nil
	if lerr := w.fio.Close(); lerr != nil && err == nil {
		err = errorLine(lerr)
	}
	return
}

func (w *Tilemap) tileid(x, y int) uint32 {
	return tileid(w.zoom, x, y)
}

// tileid maps an x, y pair to its index slot.  Tiles are stored x major so a
// column of tiles is contiguous in the index.
func tileid(zoom, x, y int) uint32 {
	return uint32(x)<<uint(zoom) | uint32(y)
}

func prepFileMap(fio *os.File, zoom int) (r int64, err error) {
	var fi os.FileInfo
	sz := int64(tileCount(zoom)) * dpsize
	if fi, err = fio.Stat(); err != nil {
		err = errorLine(err)
		return
	}
	if r = fi.Size(); r < sz {
		if err = safeFallocate(fio, r, sz); err != nil {
			return
		}
		r = sz
	}
	return
}

func checkFileMap(fio *os.File, zoom int) (r int64, err error) {
	var fi os.FileInfo
	sz := int64(tileCount(zoom)) * dpsize
	if fi, err = fio.Stat(); err != nil {
		return
	}
	if r = fi.Size(); r < sz {
		err = ErrInvalidDPRegionSize
	}
	return
}

// tileCount returns the number of tiles in a zoom level.
func tileCount(zoom int) int64 {
	return int64(1) << (2 * uint(zoom))
}

type datapointer struct {
	offset int64
	size   int64
}

// Decode pulls the 10 byte encoded datapointer out of a byte slice.
// Encoding is [48 bit little endian offset][32 bit little endian size].
func (dp *datapointer) Decode(b []byte) (err error) {
	if len(b) < dpsize {
		return ErrInvalidBufferSize
	}
	dp.offset = int64(binary.LittleEndian.Uint16(b)) | int64(binary.LittleEndian.Uint32(b[2:]))<<16
	dp.size = int64(binary.LittleEndian.Uint32(b[6:]))
	return
}

// Encode writes the datapointer into b using the layout described in Decode.
func (dp *datapointer) Encode(b []byte) (err error) {
	if len(b) < dpsize {
		return ErrInvalidBufferSize
	}
	binary.LittleEndian.PutUint16(b, uint16(dp.offset))
	binary.LittleEndian.PutUint32(b[2:], uint32(dp.offset>>16))
	binary.LittleEndian.PutUint32(b[6:], uint32(dp.size))
	return
}

func errorLine(err error) error {
	_, fname, line, _ := runtime.Caller(1)
	return fmt.Errorf("%s:%d %w", fname, line, err)
}
