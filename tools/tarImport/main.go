package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	tilemap "../.."
	"github.com/gravwell/ingesters/utils"
)

const (
	pngExt            = `.png`
	maxFileSize int64 = 1024 * 512 //512kb is the maximum tile size we allow here
)

var (
	maxZoom                 = flag.Int("max-zoom", 10, "Maximum level of zoom, must be < 20")
	ErrInvalidFileExtension = errors.New("Invalid png file extension")

	baseDir string
)

func main() {
	flag.Parse()
	if *maxZoom < 0 || *maxZoom >= 20 {
		log.Fatal("Invalid zoom level")
	}
	args := flag.Args()
	tmm := make([]*tilemap.Tilemap, *maxZoom+1)
	if len(args) != 2 {
		log.Fatalf("Invalid command, need %s <input tar> <output dir>\n", os.Args[0])
	}
	baseDir = args[1]
	if st, err := os.Stat(baseDir); err != nil {
		log.Fatalf("bad output directory %s: %v\n", baseDir, err)
	} else if !st.IsDir() {
		log.Fatalf("%s is not a directory\n", baseDir)
	}

	rdr, err := utils.OpenBufferedFileReader(args[0], 1024*1024)
	if err != nil {
		log.Fatalf("Failed to open %s: %v\n", args[0], err)
	}
	if err := tarRunner(rdr, tmm); err != nil {
		log.Fatalf("Failed to run: %v\n", err)
	}

	if err := rdr.Close(); err != nil {
		log.Fatalf("Failed to close reader: %v\n", err)
	}
	for _, v := range tmm {
		if v == nil {
			continue
		}
		if err := v.Close(); err != nil {
			log.Fatalf("Failed to close map: %v\n", err)
		}
	}
}

func tarRunner(r io.Reader, tmm []*tilemap.Tilemap) (err error) {
	tr := tar.NewReader(r)
	var hdr *tar.Header
	var zoom, x, y int
	var added uint
	bb := bytes.NewBuffer(make([]byte, maxFileSize))
	for {
		if hdr, err = tr.Next(); err != nil {
			if err == io.EOF {
				err = nil
			}
			break
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if zoom, x, y, err = processFilename(hdr.Name); err != nil {
			return
		} else if zoom < 0 || zoom >= len(tmm) {
			continue
		}

		if tmm[zoom] == nil {
			fname := filepath.Join(baseDir, fmt.Sprintf("%d.tiles", zoom))
			if tmm[zoom], err = tilemap.NewTilemap(fname, zoom, false); err != nil {
				return
			}
		}
		bb.Reset()
		io.Copy(bb, tr)
		if err = tmm[zoom].Add(x, y, bb.Bytes()); err != nil {
			return
		}
		added++
	}
	fmt.Println("Added", added, "tiles")
	return
}

func processFilename(pth string) (zoom, x, y int, err error) {
	var bits []string
	//get the zoom level, x, and y value
	if bits = strings.SplitN(pth, `/`, 3); len(bits) != 3 {
		err = fmt.Errorf("Invalid filepath: %v %v", pth, bits)
		return
	} else if !strings.HasSuffix(bits[2], pngExt) {
		err = fmt.Errorf("invalid base file name: %v", bits[2])
		return
	}
	bits[2] = strings.TrimSuffix(bits[2], pngExt)
	if zoom, err = strconv.Atoi(bits[0]); err != nil {
		return
	}

	if x, err = strconv.Atoi(bits[1]); err != nil {
		return
	} else if x > (1 << zoom) {
		err = fmt.Errorf("invalid x value: %d > %d", x, 1<<zoom)
		return
	}
	if y, err = strconv.Atoi(bits[2]); err != nil {
		return
	} else if y > (1 << zoom) {
		err = fmt.Errorf("invalid y value: %d > %d", y, 1<<zoom)
		return
	}
	return
}
