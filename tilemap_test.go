package tilemap

import (
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/dchest/siphash"
)

var (
	tdir      string
	basicBuff []byte
)

func TestMain(m *testing.M) {
	var err error
	var r int

	basicBuff = make([]byte, 1024*64)
	rand.Read(basicBuff)
	if tdir, err = ioutil.TempDir(os.TempDir(), `tilemap`); err != nil {
		fmt.Printf("Failed to create tempdir: %v", err)
		r = -1
	} else {
		r = m.Run()
	}
	os.RemoveAll(tdir)
	os.Exit(r)
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

func TestNewTilemap(t *testing.T) {
	wtr, err := NewTilemap(filepath.Join(tdir, `test`), 2, false)
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
	if err = wtr.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTilemap(t *testing.T) {
	zl := 4
	wtr, err := NewTilemap(filepath.Join(tdir, `test`), zl, false)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < zl; i++ {
		for j := 0; j < zl; j++ {
			if err = wtr.Add(i, j, randBuff(basicBuff)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err = wtr.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTilemapHighCollision(t *testing.T) {
	zl := 8
	wtr, err := NewTilemap(filepath.Join(tdir, `test`), zl, false)
	if err != nil {
		t.Fatal(err)
	}
	buff := basicBuff[0 : 8*1024]
	for i := 0; i < zl; i++ {
		for j := 0; j < zl; j++ {
			if err = wtr.Add(i, j, randBuff(buff)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err = wtr.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTilemapReadWrite(t *testing.T) {
	testMap := make(map[uint32]uint64, 1024)
	zl := 2
	wtr, err := NewTilemap(filepath.Join(tdir, `test`), zl, false)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < zl; i++ {
		for j := 0; j < zl; j++ {
			buff := randBuff(basicBuff)
			key := siphash.Hash(sipkey1, sipkey2, buff)
			if err = wtr.Add(i, j, buff); err != nil {
				t.Fatal(err)
			}
			testMap[tileid(zl, i, j)] = key
		}
	}

	for i := 0; i < zl; i++ {
		for j := 0; j < zl; j++ {
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
	if err = wtr.Close(); err != nil {
		t.Fatal(err)
	}

}

func TestTilemapReadWriteReopen(t *testing.T) {
	testMap := make(map[uint32]uint64, 1024)
	zl := 4
	wtr, err := NewTilemap(filepath.Join(tdir, `test`), zl, false)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < zl/2; i++ {
		for j := 0; j < zl; j++ {
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
	if wtr, err = NewTilemap(filepath.Join(tdir, `test`), zl, false); err != nil {
		t.Fatal(err)
	}
	for i := zl / 2; i < zl; i++ {
		for j := 0; j < zl; j++ {
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

	rdr, err := NewTilemap(filepath.Join(tdir, `test`), zl, true)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < zl; i++ {
		for j := 0; j < zl; j++ {
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
	if err = rdr.Add(0, 0, randBuff(basicBuff)); err == nil {
		t.Fatal("Failed to catch write on read only")
	}
	if err = rdr.Close(); err != nil {
		t.Fatal(err)
	}
}

func randBuff(b []byte) []byte {
	l := rand.Intn(8 * 1024)
	if l >= len(b) {
		l = len(b) - 2
	}
	off := rand.Intn(len(b) - l)
	return b[off : off+l]
}
