# tilemap

An optimized open streetmap tile store that memory maps a single file per zoom
level.  Includes tools for building tilemaps and serving them over HTTP.

## File format

Each zoom level lives in one `<zoom>.tiles` file:

```
[ 10 byte datapointer * 4^zoom ][ tile bytes ... ]
```

The index region is memory mapped, so a lookup is a bounds check and a 10 byte
decode.  Tile bytes are appended to the data region.  Identical tiles (ocean,
blank land) are stored once and shared by every tile with the same bytes; the
match is verified byte for byte before sharing so a hash collision can never
serve the wrong tile.

## Library

```go
// build
tm, err := tilemap.NewTilemap("/data/tiles/8.tiles", 8, false)
err = tm.Add(x, y, pngBytes)
err = tm.Close()

// serve
tm, err := tilemap.NewTilemap("/data/tiles/8.tiles", 8, true)
buff, err := tm.GetTile(x, y) // tilemap.ErrTileNotFound for a tile that was never added
ok, err := tm.Has(x, y)

// check an existing file before resuming it
info, err := tilemap.ScanIndex("/data/tiles/8.tiles", 8) // tilemap.ErrZoomMismatch on a mismatched file
fmt.Println(info.Present, info.Tiles, info.Complete())
```

Writers buffer appends and flush on `Flush`, `Sync`, `Close`, or whenever a
read needs bytes that are still buffered.  Read only handles map the whole
file and serve tiles straight out of the page cache.

## Tools

The tools live in their own module under `tools/` so the library does not
pull in their dependencies.  Build them from that directory.

### httpClient

Builds the tilemap for one zoom level by requesting every tile from a tile
server.

```
cd tools && go build ./httpClient
./httpClient -endpoint http://tiles.example.com/tile/{z}/{x}/{y}.png -zoom 8 -output /data/tiles -parallelism 16
```

| flag | default | description |
| --- | --- | --- |
| `-endpoint` | | tile server URL, `{z}`, `{x}` and `{y}` are substituted; with no placeholders `/{z}/{x}/{y}.png` is appended |
| `-zoom` | | zoom level to generate, 0 to 16 |
| `-output` | | directory that receives `<zoom>.tiles` |
| `-parallelism` | 8 | concurrent requests |
| `-timeout` | 2m | per request timeout |
| `-check-timeout` | 30s | timeout for the startup request that checks the endpoint, raise it for slow on-demand renderers |
| `-retries` | 10 | retries per tile on network errors, 5xx, 429 and 408 (0 retries forever) |
| `-resume` | true | skip tiles already present in an existing `<zoom>.tiles` |
| `-allow-missing` | false | treat 404 as an empty tile instead of failing |
| `-user-agent` | gravwell-tilemap/1.0 | User-Agent header |

Tiles are requested in 8x8 blocks so rendering backends that produce metatiles
serve most of a block from cache.

Runs can be stopped and restarted.  Re-running with the same arguments picks
up where the last run left off:

1. An existing `<zoom>.tiles` is scanned with `tilemap.ScanIndex` before it is
   opened for writing.  The file must be laid out for the requested zoom (see
   below), otherwise the run stops without touching it.
2. Every tile already in the index is skipped, only missing tiles are
   requested.  A map that is already complete exits without contacting the
   server.

An interrupted run, including one killed with `kill -9`, leaves a consistent
tilemap.  Buffered tiles are flushed every 10 seconds, so a hard kill loses at
most the last few seconds of work; tiles that never reached the file are
never referenced by the index.  Tiles skipped with `-allow-missing` are not
stored, so they are requested again on every resume.

The file format does not record its zoom level, so `ScanIndex` checks the
structure instead: the file must be at least as large as the zoom level's
index, every index entry must be empty or point inside the data region, and
the lowest referenced tile must sit right after the index, where the first
tile added is always written.  A file built for a different zoom fails one
of these checks.

### webserver

Serves every `<zoom>.tiles` file found under a directory at
`/tiles/{zoom}/{x}/{y}.png`.  See `tools/webserver/config.json` for the
configuration file and `Dockerfile` for a container build.

### tarImport

Imports tiles from a tar archive laid out as `<zoom>/<x>/<y>.png`.

```
./tarImport -max-zoom 15 tiles.tar /data/tiles
```
