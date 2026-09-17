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
          workstation (this repo runs here)
 ┌──────────────────────────────────────────────┐
 │ browser ──userscript──► shokod :7373 (lo)     │   http    ┌────────────────────┐
 │                          │   │   │            │ ◄───────► │ Shoko Server (poco)│
 │                          │   │   stream proxy │ /api/v3   └────────────────────┘
 │                          │   └── mpv (ipc)    │
 │                          │        ▲           │           ┌────────────────────┐
 │                          └── syncplay client ─┼─────────► │ syncplay server    │
 │                              (official, ini)  │           │ (anywhere, e.g.    │
 │                                               │           │  syncplay.pl)      │
 │                            anilist module ────┼─────────► api.anilist.co       │
 └──────────────────────────────────────────────┘           └────────────────────┘
                                                   friends: plain syncplay ──► same server
```

## Why Go

- `CGO_ENABLED=0 go build` = one static binary, no runtime. Cross-compiles to
  linux/arm64 (termux/proot) and windows with two env vars.
- Everything needed is stdlib: `net/http` (Shoko/AniList/proxy), `net` unix
  socket (mpv JSON IPC), `os/exec` (syncplay), `encoding/json`.
- Rejected: Rust (slower iteration, same result), Python (no static build).

## Layout (single repo)

```
cmd/shokod/main.go          everything daemon-side, one file until it hurts
userscript/shoko-syncplay.user.js
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
  `syncplay --no-gui --player-path mpv --load-playlist-from-file shokod.m3u
   [-a H -r R -n N] <first entry> -- --input-ipc-server=$XDG_RUNTIME_DIR/shokod-mpv.sock`.
  Host/room/name fall back to syncplay.ini when flags unset. `--` is required
  before mpv args. If syncplay already running: kill + respawn (lazy; replace
  with playlist-append later if it annoys).
- Verified live: proxy streams with Range, syncplay opens entry 1, advances
  to entry 2 at end-of-file, mpv IPC socket answers on our path.
- Friends with plain Syncplay: they see the playlist names, open their own
  file by hand. Play/pause/seek sync works regardless; Syncplay only *warns*
  on name/size/duration mismatch. Auto-advance won't work for them (127.0.0.1
  URL is unplayable on their box). Accepted.
- Solo watching = room of one. No separate mpv-only path.
- Syncplay config needs `127.0.0.1` in `trustedDomains` (syncplay.ini) so it
  auto-advances to the proxy URLs without prompting.

### 2. WebUI userscript

`userscript/shoko-syncplay.user.js` (Violentmonkey/Tampermonkey; edit `@match`
to your Shoko URL). Floating panel bottom-right on `/series/<id>` pages, built
from `GET /episodes?series=<id>` (daemon does the Shoko call, so no WebUI DOM
or apikey dependency). "ep"/"series" buttons -> `/syncplay?episode=<id>&mode=`.
Verified in Firefox.
Daemon answers CORS `Access-Control-Allow-Origin: <SHOKO_URL origin>`.
Chrome Private Network Access may need
`chrome://flags/#block-insecure-private-network-requests` off; Firefox fine.

### 3. Watched -> Shoko

- Every 5s over mpv IPC: `get_property path` + `percent-pos`. Path tells which
  fileID is playing (parsed from the proxy URL), so playlist advance is free.
- `percent-pos >= 85` (flag `-watched-at`) -> `POST /api/v3/File/<id>/Watched/true`
  once per file (path param; `?watched=` is silently ignored by Shoko). Then
  AniList sync for that series.
- v2 (skip): `File/<id>/Scrobble` for resume position.

### 4. AniList sync

One-way Shoko -> AniList.

- Token: implicit grant, no code. Create an API client at
  anilist.co/settings/developer (redirect `https://anilist.co/api/v2/oauth/pin`),
  open `https://anilist.co/api/v2/oauth/authorize?client_id=<ID>&response_type=token`,
  paste token into `ANILIST_TOKEN`. Token lives ~1 year.
- Mapping: AniDB series id -> AniList id via Fribb/anime-lists
  `anime-list-full.json`, cached `~/.cache/shokod-anime-list.json`, refreshed weekly.
- Progress = highest watched ep number where every lower ep *that has a file*
  is watched (missing early eps assumed seen elsewhere). AniDB specials
  ignored. progress >= AniList episode count -> `COMPLETED`, else `CURRENT`.
- Triggers: after each watched mark (module 3), and `shokod anilist-sync`
  one-shot over all Shoko series. Per series: one `Media{mediaListEntry}`
  query, one `SaveMediaListEntry` only if Shoko is ahead. Never lowers
  AniList progress -> rewatching a COMPLETED series is a no-op.

## Config

Flags with env fallback, no config lib:

```
-listen     127.0.0.1:7373   SHOKOD_LISTEN
-shoko      http://poco:8111 SHOKO_URL
-shoko-key                   SHOKO_APIKEY     (Shoko WebUI -> settings -> API keys)
-watched-at 85               (flag only)
-sp-host    syncplay.pl:8997 SYNCPLAY_HOST
-sp-room                     SYNCPLAY_ROOM
-sp-name                     SYNCPLAY_NAME
-anilist-token               ANILIST_TOKEN
```

Handy: keep secrets in `~/.config/shokod.env` (0600) and run
`set -a; . ~/.config/shokod.env; set +a; shokod`.

## Build

```sh
make            # ./dist/shokod-linux-amd64 -linux-arm64 -windows-amd64.exe
```

## Status

All four modules built and verified live (2026-09-17). Remaining: real-world
use, then whatever annoys.

## Later / maybe

- Shoko reachable over private VPN: nothing changes here, `SHOKO_URL` just
  points at the VPN address. Proxy still needed (api key).

## Verified against live Shoko 5.3.3

- `File/{id}/Stream` accepts `apikey` header or query, honors Range (206).
- `includeDataFrom=AniDB` gives `AniDB.Type` / `AniDB.EpisodeNumber`; `pageSize=0` = all.
- `File/{id}/Watched/{bool}` path param; query form is ignored.
