package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gravwell/ingesters/v3/utils"
	"github.com/gravwell/tilemap/v1"
)

func main() {
	if len(os.Args) != 2 {
		log.Fatalf("config file required: %s <config file>\n", os.Args[0])
	}
	cfg, err := LoadConfig(os.Args[1])
	if err != nil {
		log.Fatalf("Failed to load config: %v\n", err)
	}

	//load up our tile maps
	tmm, err := loadTilemaps(cfg.MapDir)
	if err != nil {
		log.Fatalf("Failed to gather tilemaps: %v\n", err)
	}

	ws, err := NewWebserver(cfg, tmm)
	if err != nil {
		log.Fatalf("Failed to start the webserver: %v\n", err)
	}

	if err := ws.Start(); err != nil {
		closeTilemaps(tmm)
		log.Fatalf("Failed to start webserver: %v\n", err)
	}

	utils.WaitForQuit() //wait for one of our shutdown signals

	if err := ws.Close(); err != nil {
		log.Fatalf("Failed to close webserver")
	}

	if err := closeTilemaps(tmm); err != nil {
		log.Fatalf("Failed to close tilemaps: %v\n", err)
	}
}

func closeTilemaps(tmm []*tilemap.Tilemap) (err error) {
	for i, v := range tmm {
		if v != nil {
			if lerr := v.Close(); lerr != nil {
				err = fmt.Errorf("Failed to close tilemap %d: %v", i, lerr)
			}
		}
	}
	return
}

func loadTilemaps(pth string) (tmm []*tilemap.Tilemap, err error) {
	tmm = make([]*tilemap.Tilemap, 21) //space for the full zoom level of 20
	err = filepath.Walk(pth, func(path string, info os.FileInfo, lerr error) error {
		if lerr != nil {
			return lerr
		} else if !info.Mode().IsRegular() {
			return nil //skip non-regular files
		} else if filepath.Ext(path) != TilesExtension {
			return nil //skip anything that doesn't have our tiles extension
		}

		//check the filename
		fn := filepath.Base(path)
		fn = strings.TrimSuffix(fn, TilesExtension)
		zoom, lerr := strconv.Atoi(fn)
		if lerr != nil {
			return fmt.Errorf("Bad tilemap file name %q: %v", path, lerr)
		}
		if zoom < 0 || zoom > maxZoom {
			return fmt.Errorf("Bad tilemap file zoom level: %d must be between 0 and 20", zoom)
		}
		//check that we haven't already loaded this zoom level
		if tmm[zoom] != nil {
			return fmt.Errorf("Tilemap zoom level of %d is already loaded", zoom)
		}
		tmm[zoom], lerr = tilemap.NewTilemap(path, zoom, true) //open it read only
		return lerr
	})
	if err != nil {
		for _, v := range tmm {
			if v != nil {
				v.Close()
			}
		}
		return
	}

	//loop through and trim our slice
	for len(tmm) > 0 {
		if tmm[len(tmm)-1] == nil {
			tmm = tmm[:len(tmm)-1]
		} else {
			break
		}
	}
	return
}
