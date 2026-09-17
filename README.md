# shoko-companion

External companion daemon for [Shoko Server](https://shokoanime.com). One static
Go binary (`shokod`) + one browser userscript. Not a Shoko .NET plugin: runs on
the *client* machine (the one with mpv/syncplay), talks to Shoko over its v3
HTTP API only. No mounts, no path mapping.

## Goal

1. User opens Shoko WebUI, clicks an episode -> button "ep" or "series".
2. Syncplay + mpv open. "ep" = that one episode. "series" = playlist of every
   episode of the series that has a file, starting at the clicked one.
3. Friends join the Syncplay room with plain Syncplay, no Shoko, no daemon.
4. Watched state flows back: mpv -> Shoko -> AniList.

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
- Everything needed is stdlib: `net/http` (Shoko/AniList/proxy), `net` unix
  socket (mpv JSON IPC), `os/exec` (syncplay), `encoding/json`.
- Rejected: Rust (slower iteration, same result), Python (no static build).

## Layout (single repo)

```
cmd/shokod/main.go          flags/env, HTTP server on 127.0.0.1:7373, wires modules
internal/shoko/             v3 API client: auth, series/episode/file lookup, watched, stream
internal/syncplay/          build playlist file, spawn `syncplay --no-gui ... mpv`
internal/mpv/               poll percent-pos over --input-ipc-server, fire watched events
internal/anilist/           GraphQL client, MediaListCollection import, SaveMediaListEntry
internal/anilist/mapping/   Fribb/anime-lists anime-list-full.json loader (anidb -> anilist)
userscript/shoko-play-in-mpv.user.js
Makefile                    static builds: linux/amd64, linux/arm64, windows/amd64
```

## Modules

### 1. Syncplay launch + stream proxy

- `GET /syncplay?episode=<shokoEpisodeID>&mode=ep|series`
- series mode: `GET /api/v3/Series/<id>/Episode?includeFiles=true`, keep
  episodes with >=1 file, sorted by episode number, rotate so clicked one is
  first. ep mode: just that one.
- Playlist entries are **local proxy URLs**
  `http://127.0.0.1:7373/stream/<fileID>/<Series - 05.mkv>`. Proxy forwards to
  `SHOKO/api/v3/File/<id>/Stream` and injects `apikey` server-side.
  Reason: syncplay broadcasts the path to the room; the real Shoko URL would
  leak the api key. Readable name in the path is what friends see.
- Write `$XDG_RUNTIME_DIR/shokod.m3u`, spawn
  `syncplay --no-gui --host H --room R --name N --player-path mpv
   --load-playlist-from-file shokod.m3u -- --input-ipc-server=$XDG_RUNTIME_DIR/shokod-mpv.sock`.
  If syncplay already running: kill + respawn (lazy; replace with
  playlist-append later if it annoys).
- Friends with plain Syncplay: they see the playlist names, open their own
  file by hand. Play/pause/seek sync works regardless; Syncplay only *warns*
  on name/size/duration mismatch. Auto-advance won't work for them (127.0.0.1
  URL is unplayable on their box). Accepted.
- Solo watching = room of one. No separate mpv-only path.
- Syncplay config needs `127.0.0.1` in `trustedDomains` so it opens the proxy
  URLs without prompting. Daemon prints the ini snippet on first run.

### 2. WebUI userscript

`userscript/shoko-play-in-mpv.user.js` (Violentmonkey/Tampermonkey).
Two small buttons per episode row in Shoko WebUI: "ep", "series" ->
`fetch('http://127.0.0.1:7373/syncplay?episode=<id>&mode=...')`.
Daemon answers CORS `Access-Control-Allow-Origin: <SHOKO_URL origin>`.
Chrome Private Network Access may need
`chrome://flags/#block-insecure-private-network-requests` off; Firefox fine.

### 3. Watched -> Shoko

- Every 5s over mpv IPC: `get_property path` + `percent-pos`. Path tells which
  fileID is playing (parsed from the proxy URL), so playlist advance is free.
- `percent-pos >= 85` (flag `-watched-at`) -> `POST /api/v3/File/<id>/Watched?watched=true`
  once per file. Emits event to AniList module.
- v2 (skip): `File/<id>/Scrobble` for resume position.

### 4. AniList sync

One-way Shoko -> AniList.

- Token: implicit grant. `shokod anilist login` prints the
  `anilist.co/api/v2/oauth/authorize?client_id=...&response_type=token` URL,
  user pastes token back. Stored `0600`.
- Mapping: AniDB series id -> AniList id via Fribb/anime-lists
  `anime-list-full.json`, cached `~/.cache/shokod/`, refreshed weekly.
- Progress = highest N such that eps 1..N all watched (contiguous). AniDB
  specials ignored. N == episode count -> `COMPLETED`, else `CURRENT`.
- Triggers: event from module 3 (debounced 10s/series, one
  `SaveMediaListEntry`), and `shokod anilist sync` one-shot: paginated
  `MediaListCollection` pull, push only where Shoko is ahead. Never lowers
  AniList progress.

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

1. module 1: Shoko client + playlist + proxy + syncplay spawn. Test with curl.
2. module 2: userscript. Now the goal flow works end to end.
3. module 3: watched -> Shoko.
4. module 4: AniList.

## Later / maybe

- Shoko reachable over private VPN: nothing changes here, `SHOKO_URL` just
  points at the VPN address. Proxy still needed (api key).

## Open / verify against live Shoko

- exact v3 stream endpoint (`File/{id}/Stream` vs `StreamDirectory`) and
  whether `apikey` query/header is accepted there; whether it honors Range.
- episode type field for filtering specials.
- `--load-playlist-from-file` start index behaviour (rotate list vs `--file`).
