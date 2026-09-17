# shoko-companion

External companion daemon for [Shoko Server](https://shokoanime.com). One static
Go binary (`shokod`) + one browser userscript. Not a Shoko .NET plugin: runs on
the *client* machine (the one with mpv/syncplay), talks to Shoko over its v3
HTTP API only. No mounts, no path mapping.

```
          workstation (this repo runs here)                 poco (proot-debian)
 ┌──────────────────────────────────────────────┐        ┌──────────────────┐
 │ browser ──userscript──► shokod :7373 (lo)     │        │                  │
 │                          │   │   │            │  http  │  Shoko Server    │
 │                          │   │   └─ anilist ──┼──────────► api.anilist.co │
 │                          │   └── mpv (ipc)    │        │                  │
 │                          └── syncplay client ─┼──────────► syncplay srv   │
 │                    stream proxy ◄─────────────┼──────────  /api/v3/File   │
 └──────────────────────────────────────────────┘        └──────────────────┘
```

## Why Go

- `CGO_ENABLED=0 go build` = one static binary, no runtime. Cross-compiles to
  linux/arm64 (termux/proot) and windows with two env vars.
- Everything needed is stdlib: `net/http` (Shoko/AniList), `net` unix socket
  (mpv JSON IPC), `os/exec` (mpv, syncplay), `encoding/json`.
- Rejected: Rust (slower iteration, same result), Python (no static build,
  needs a runtime on every box).

## Layout (single repo)

```
cmd/shokod/main.go          flags/env, HTTP server on 127.0.0.1:7373, wires modules
internal/shoko/             v3 API client: auth, episode/file lookup, watched, stream URL
internal/mpv/               spawn mpv --input-ipc-server, poll percent-pos, observe end-file
internal/syncplay/          spawn `syncplay --no-gui`, parse stdout, resolve room file, chat hints
internal/anilist/           GraphQL client, MediaListCollection import, SaveMediaListEntry
internal/anilist/mapping/   Fribb/anime-lists anime-list-full.json loader (anidb -> anilist), disk cached
userscript/shoko-play-in-mpv.user.js
Makefile                    static builds: linux/amd64, linux/arm64, windows/amd64
```

## Modules

### 1. mpv control (`/play`)

- `GET /play?file=<shokoFileID>` (or `?episode=<shokoEpisodeID>` -> first file).
- Daemon starts `mpv --input-ipc-server=$XDG_RUNTIME_DIR/shokod-mpv.sock <url>`.
  Reuses running mpv via `loadfile` if socket alive.
- `<url>` is the **local stream proxy** `http://127.0.0.1:7373/stream/<fileID>/<Series - 05.mkv>`.
  Proxy forwards to `SHOKO/api/v3/File/<id>/Stream` and injects `apikey`.
  Reason: mpv/syncplay broadcast the path to the room; the real Shoko URL would
  leak the api key. Proxy path also gives peers a readable filename.
- Every 5s: `get_property percent-pos`. `>= 85%` (flag `-watched-at`) ->
  `POST /api/v3/File/<id>/Watched?watched=true` once. Also emits event to
  AniList module.
- v2 (skip for now): `File/<id>/Scrobble` for resume position.

### 2. Syncplay (model 2)

Constraint: peers may run plain Syncplay with no Shoko and no daemon. So the
official `syncplay` client stays the sync engine, unmodified. Daemon only
(a) picks the local file and (b) listens.

- `GET /syncplay?file=<id>` -> spawn
  `syncplay --no-gui --host H --room R --name N --player-path mpv --file <proxy url> -- --input-ipc-server=...`.
  mpv IPC observation (module 1) works the same, so watched state flows too.
- Daemon reads syncplay stdout. Line `"<user> is playing '<name>' (<size>)"`
  = room switched file. Resolution order:
  1. Chat hint `shoko:anidb-ep=<id>` posted by a daemon-equipped peer -> exact.
     Plain peers just see one harmless chat line.
  2. Filename parse (series title + ep number, regex, anitomy-style) ->
     `GET /api/v3/Series/Search/<title>` + episode number -> Shoko file.
  3. No match -> log + OSD text via mpv `show-text`, user picks manually
     (`/play`). Nothing breaks, syncplay keeps working on whatever mpv has.
- Match found -> `loadfile` in the running mpv (syncplay follows, it watches
  mpv's `path`). Post our own chat hint after switching.
- Release differences: Syncplay only *warns* on name/size/duration mismatch,
  never blocks. Set `filenamePrivacyMode` untouched; size from proxy is
  `Content-Length` of the real file so it's honest, just different.

### 3. AniList sync

One-way Shoko -> AniList.

- Token: implicit grant. `shokod anilist login` prints the
  `anilist.co/api/v2/oauth/authorize?client_id=...&response_type=token` URL,
  user pastes token back. Stored in config file, `0600`.
- Mapping: AniDB series id -> AniList id via Fribb/anime-lists
  `anime-list-full.json`, downloaded on first use, cached `~/.cache/shokod/`,
  refreshed weekly.
- Progress rule: `progress = highest N such that eps 1..N are all watched`
  (contiguous). AniDB specials (type != regular) ignored. If N == series
  episode count -> `status: COMPLETED`, else `CURRENT`.
- Triggers:
  - event from module 1 after marking watched (debounced 10s per series,
    one `SaveMediaListEntry` mutation).
  - `shokod anilist sync` one-shot: paginated `MediaListCollection` pull,
    diff against Shoko per-series watched state, push only where Shoko is ahead.
    Never lowers AniList progress.

### 4. WebUI userscript

`userscript/shoko-play-in-mpv.user.js` (Violentmonkey/Tampermonkey).
Injects a "mpv" button next to episodes/files in Shoko WebUI, calls
`http://127.0.0.1:7373/play?file=<id>` (and a "syncplay" button -> `/syncplay`).
Daemon answers CORS `Access-Control-Allow-Origin: <SHOKO_URL origin>`.
Chrome Private Network Access may need
`chrome://flags/#block-insecure-private-network-requests` off; Firefox fine.

## Config

Flags with env fallback, no config lib:

```
-listen     127.0.0.1:7373   SHOKOD_LISTEN
-shoko      http://poco:8111 SHOKO_URL
-shoko-key                   SHOKO_APIKEY     (from POST /api/auth once, see `shokod login`)
-watched-at 85               SHOKOD_WATCHED_AT
-sp-host    syncplay.pl:8997 SYNCPLAY_HOST
-sp-room                     SYNCPLAY_ROOM
-sp-name                     SYNCPLAY_NAME
-anilist-token               ANILIST_TOKEN
```

## Build

```sh
make            # ./dist/shokod-linux-amd64 -linux-arm64 -windows-amd64.exe
```

## Order of work

1. `internal/shoko` + `/play` + stream proxy + watched POST  (module 1)
2. userscript  (module 4, 50 lines, makes 1 usable)
3. AniList  (module 3)
4. Syncplay  (module 2, the novel part, builds on 1)

## Open / to verify against live Shoko

- exact v3 stream endpoint (`File/{id}/Stream` vs `StreamDirectory`) and
  whether `apikey` query param is accepted there.
- episode type field for filtering specials.
- `Series/Search` fuzziness good enough for filename titles.
