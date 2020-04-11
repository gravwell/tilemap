package tilemap

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"

	"github.com/dchest/siphash"
	"github.com/tysontate/gommap"
)

const (
	dpsize         = 6 + 4 //48bit offset, 4 byte size
	maxDimension   = 16
	maxMapInitSize = 1024 * 1024 * 16 //start at 16mil
	maxTileSize    = 1 << 24          // like 4MB whichi si stupid

	MaxZoom = maxDimension

	wtrMapFlags  = gommap.PROT_WRITE | gommap.PROT_READ
	wtrOpenFlags = os.O_RDWR | os.O_CREATE
	rdrMapFlags  = gommap.PROT_READ
	rdrOpenFlags = os.O_RDONLY
)

var (
	ErrInvalidBufferSize   = errors.New("Invalid buffer size")
	ErrInvalidDimension    = errors.New("Invalid tile zoom")
	ErrInvalidTileID       = errors.New("invalid tile id")
	ErrInvalidTileBuffer   = errors.New("invalid tile buffer")
	ErrInvalidDatapointer  = errors.New("invalid tile datapointer, file may be corrupt")
	ErrInvalidDPRegionSize = errors.New("invalid tile datapointer region size, file may be corrupt")
	ErrPartialWrite        = errors.New("Partial write")
)

type Tilemap struct {
	sync.RWMutex
	ro   bool
	zoom int
	hmp  map[uint64]uint32
	fio  *os.File
	mm   gommap.MMap
	foff int64
}

func NewTilemap(pth string, zoom int, ro bool) (w *Tilemap, err error) {
	if zoom < 0 || zoom > maxDimension {
		err = ErrInvalidDimension
		return
	} else if pth == `` {
		err = errors.New("invalid path")
		return
	}
	var oflags int
	var mapflags gommap.ProtFlags

	if ro {
		oflags = rdrOpenFlags
		mapflags = rdrMapFlags
	} else {
		oflags = wtrOpenFlags
		mapflags = wtrMapFlags

	}

	var fio *os.File
	var mm gommap.MMap
	tc := tileCount(zoom)
	dpRegionSize := int64(tc * dpsize)
	if fio, err = os.OpenFile(pth, oflags, 0640); err != nil {
		return
	}
	//best effort, may not be supported
	setAttr(fio, NO_COW)

	// if not in readonly mode, prep the file
	if !ro {
		if _, err = prepFileMap(fio, zoom); err != nil {
			fio.Close()
			return
		}
	} else {
		if _, err = checkFileMap(fio, zoom); err != nil {
			fio.Close()
			return
		}
	}
	if mm, err = gommap.MapRegion(fio.Fd(), 0, dpRegionSize, mapflags, gommap.MAP_SHARED); err != nil {
		fio.Close()
	}
	var foff int64
	if foff, err = fio.Seek(0, os.SEEK_END); err != nil {
		mm.UnsafeUnmap()
		fio.Close()
		return
	} else if foff < dpRegionSize {
		mm.UnsafeUnmap()
		fio.Close()
		err = ErrInvalidDPRegionSize
		return
	}
	w = &Tilemap{
		ro:   ro,
		zoom: zoom,
		fio:  fio,
		mm:   mm,
		foff: foff,
	}
	return
}

func (w *Tilemap) initHashMap() {
	mapInitSize := tileCount(w.zoom)
	if mapInitSize > maxMapInitSize {
		mapInitSize = maxMapInitSize
	}
	w.hmp = make(map[uint64]uint32, mapInitSize)
}

func (w *Tilemap) Add(x, y int, buff []byte) (err error) {
	w.Lock()
	defer w.Unlock()
	var dp datapointer
	bsize := len(buff)
	mid := 1 << uint(w.zoom)
	tid := w.tileid(x, y)
	if x < 0 || x >= mid || y < 0 || y >= mid {
		err = fmt.Errorf("%v %d %d", errorLine(ErrInvalidTileID), x, y)
		return
	} else if bsize == 0 || bsize > maxTileSize {
		err = errorLine(ErrInvalidTileBuffer)
		return
	}
	if w.hmp == nil {
		//we are going to be writing, so go ahead and init the hash map
		w.initHashMap()
	}
	//hash the buffer and check if we know about it
	key := siphash.Hash(sipkey1, sipkey2, buff)
	if extid, ok := w.hmp[key]; ok {
		if dp, err = w.getDataPointer(extid); err != nil {
			err = errorLine(err)
			return
		}
	} else {
		//no hash collision, write it out
		if dp, err = w.writeNewBuffer(buff); err != nil {
			err = errorLine(err)
			return
		}
		w.hmp[key] = tid //add to our collision map
	}
	if err = w.setDataPointer(tid, dp); err != nil {
		err = fmt.Errorf("Failed to set datapointer for %d/%d: %v", x, y, err)
	}
	return
}

func (w *Tilemap) writeNewBuffer(buff []byte) (dp datapointer, err error) {
	var n int
	if n, err = w.fio.Write(buff); err != nil {
		return
	} else if n != len(buff) {
		err = errorLine(ErrPartialWrite)
		return
	}
	dp.offset = w.foff
	dp.size = int64(len(buff))
	w.foff += dp.size
	return
}

func (w *Tilemap) getDataPointer(tid uint32) (dp datapointer, err error) {
	toff := int64(tid) * dpsize
	if (toff + 6) > int64(len(w.mm)) {
		err = errorLine(ErrInvalidTileID)
		err = fmt.Errorf("%v %d > %d", err, (toff + 6), len(w.mm))
	} else {
		if err = dp.Decode(w.mm[toff:]); err != nil {
			err = errorLine(err)
		}
	}
	return
}

func (w *Tilemap) setDataPointer(tid uint32, dp datapointer) (err error) {
	toff := int64(tid) * dpsize
	if (toff + 6) > int64(len(w.mm)) {
		err = fmt.Errorf("%v %d %d > %d", errorLine(ErrInvalidTileID), tid, (toff + 6), len(w.mm))
	} else {
		if err = dp.Encode(w.mm[toff:]); err != nil {
			err = errorLine(err)
		}
	}
	return
}

func (w *Tilemap) GetTile(x, y int) (buff []byte, err error) {
	var dp datapointer
	w.RLock()
	defer w.RUnlock()
	tid := w.tileid(x, y)
	if dp, err = w.getDataPointer(tid); err != nil {
		err = errorLine(err)
		return
	}
	//check that the bounds of the buffer are valid
	buffStart := dp.offset
	buffEnd := dp.offset + dp.size
	if buffStart < int64(len(w.mm)) || buffEnd > w.foff {
		err = errorLine(fmt.Errorf("%v %x:%x %x:%x %v",
			buffStart, buffEnd, len(w.mm), w.foff, ErrInvalidDatapointer))
	} else {
		buff = make([]byte, dp.size)
		if _, err = w.fio.ReadAt(buff, dp.offset); err != nil {
			buff = nil
			err = errorLine(err)
		}
	}
	return
}

func (w *Tilemap) Close() (err error) {
	w.hmp = nil
	if err = w.mm.UnsafeUnmap(); err != nil {
		w.fio.Close()
		err = errorLine(err)
	} else {
		if err = w.fio.Close(); err != nil {
			err = errorLine(err)
		}
	}
	return
}

func (w *Tilemap) tileid(x, y int) uint32 {
	return tileid(w.zoom, x, y)
}

func tileid(zoom, x, y int) uint32 {
	dim := int(1 << uint(zoom))
	if dim == 1 {
		return 0 //zoom of zero only has one tile, this is a special case
	}
	return uint32(x*dim + y)
}

func prepFileMap(fio *os.File, d int) (r int64, err error) {
	var fi os.FileInfo
	tc := tileCount(d)
	sz := int64(dpsize * tc)
	if fi, err = fio.Stat(); err != nil {
		err = errorLine(err)
		return
	}
	if fi.Size() < sz {
		if err = safeFallocate(fio, fi.Size(), sz); err == nil {
			r = sz
		}
	} else {
		r = fi.Size()
	}
	return
}

func checkFileMap(fio *os.File, d int) (r int64, err error) {
	var fi os.FileInfo
	tc := tileCount(d)
	sz := int64(dpsize * tc)
	if fi, err = fio.Stat(); err != nil {
		return
	}
	if fi.Size() < sz {
		err = errors.New("File map is undersized")
	}
	return
}

func tileCount(zoom int) int64 {
	x := int64(1 << int64(zoom))
	return x * x
}

type datapointer struct {
	offset int64
	size   int64
}

// Decode pulls the 10bit encoded databpointer out of a byte slice
// encoding is [48 bits] [32bits]
func (dp *datapointer) Decode(b []byte) (err error) {
	if len(b) < dpsize {
		err = ErrInvalidBufferSize
	} else {
		dp.offset = int64(binary.LittleEndian.Uint16(b))
		dp.offset |= (int64(binary.LittleEndian.Uint32(b[2:])) << 16)
		dp.size = int64(binary.LittleEndian.Uint32(b[6:]))
	}
	return
}

func (dp *datapointer) Encode(b []byte) (err error) {
	if len(b) < dpsize {
		err = ErrInvalidBufferSize
	} else {
		binary.LittleEndian.PutUint16(b, uint16(dp.offset))
		binary.LittleEndian.PutUint32(b[2:], uint32(dp.offset>>16))
		binary.LittleEndian.PutUint32(b[6:], uint32(dp.size))
	}
	return
}

func errorLine(err error) error {
	_, fname, line, _ := runtime.Caller(1)
	return fmt.Errorf("%s:%d %v", fname, line, err)
}
