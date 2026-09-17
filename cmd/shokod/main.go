package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

var (
	listen       = flag.String("listen", env("SHOKOD_LISTEN", "127.0.0.1:7373"), "listen address")
	shokoURL     = flag.String("shoko", env("SHOKO_URL", "http://127.0.0.1:8111"), "Shoko Server URL")
	shokoKey     = flag.String("shoko-key", env("SHOKO_APIKEY", ""), "Shoko API key")
	spHost       = flag.String("sp-host", env("SYNCPLAY_HOST", ""), "syncplay host:port (default: syncplay.ini)")
	spRoom       = flag.String("sp-room", env("SYNCPLAY_ROOM", ""), "syncplay room (default: syncplay.ini)")
	spName       = flag.String("sp-name", env("SYNCPLAY_NAME", ""), "syncplay username (default: syncplay.ini)")
	watchedAt    = flag.Float64("watched-at", 85, "mark watched in Shoko at this percent-pos")
	anilistToken = flag.String("anilist-token", env("ANILIST_TOKEN", ""), "AniList access token (implicit grant)")
	runDir       = env("XDG_RUNTIME_DIR", os.TempDir())
)

type episode struct {
	IDs   struct{ ID, ParentSeries int }
	Name  string
	AniDB struct {
		Type          string
		EpisodeNumber int
	}
	Files   []struct{ ID int }
	Watched *string
}

func allEpisodes(seriesID int) ([]episode, error) {
	var page struct{ List []episode }
	err := shokoGet(fmt.Sprintf("/api/v3/Series/%d/Episode?pageSize=0&includeFiles=true&includeDataFrom=AniDB", seriesID), &page)
	var eps []episode
	for _, e := range page.List {
		if e.AniDB.Type == "Episode" {
			eps = append(eps, e)
		}
	}
	sort.Slice(eps, func(i, j int) bool { return eps[i].AniDB.EpisodeNumber < eps[j].AniDB.EpisodeNumber })
	return eps, err
}

func seriesEpisodes(seriesID int) ([]episode, error) {
	all, err := allEpisodes(seriesID)
	var eps []episode
	for _, e := range all {
		if len(e.Files) > 0 {
			eps = append(eps, e)
		}
	}
	return eps, err
}

func shokoGet(path string, out any) error {
	req, _ := http.NewRequest("GET", *shokoURL+path, nil)
	req.Header.Set("apikey", *shokoKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("shoko %s: %s", path, res.Status)
	}
	return json.NewDecoder(res.Body).Decode(out)
}

func playlist(episodeID int, mode string) ([]string, error) {
	var ep episode
	if err := shokoGet(fmt.Sprintf("/api/v3/Episode/%d?includeFiles=true&includeDataFrom=AniDB", episodeID), &ep); err != nil {
		return nil, err
	}
	var series struct{ Name string }
	if err := shokoGet(fmt.Sprintf("/api/v3/Series/%d", ep.IDs.ParentSeries), &series); err != nil {
		return nil, err
	}
	eps := []episode{ep}
	if mode == "series" {
		var err error
		if eps, err = seriesEpisodes(ep.IDs.ParentSeries); err != nil {
			return nil, err
		}
		eps = startAt(eps, episodeID)
	}
	var out []string
	for _, e := range eps {
		if len(e.Files) == 0 {
			return nil, fmt.Errorf("episode %d has no file", e.IDs.ID)
		}
		name := fmt.Sprintf("%s - %02d.mkv", series.Name, e.AniDB.EpisodeNumber)
		out = append(out, fmt.Sprintf("http://%s/stream/%d/%s", *listen, e.Files[0].ID, url.PathEscape(name)))
	}
	return out, nil
}

func startAt(eps []episode, episodeID int) []episode {
	for i, e := range eps {
		if e.IDs.ID == episodeID {
			return append(eps[i:], eps[:i]...)
		}
	}
	return eps
}

var (
	spMu  sync.Mutex
	spCmd *exec.Cmd
)

func launch(entries []string) error {
	spMu.Lock()
	defer spMu.Unlock()
	if spCmd != nil && spCmd.ProcessState == nil {
		spCmd.Process.Kill()
		spCmd.Wait()
	}
	pl := filepath.Join(runDir, "shokod.m3u")
	if err := os.WriteFile(pl, []byte(strings.Join(entries, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	args := []string{"--no-gui", "--player-path", "mpv", "--load-playlist-from-file", pl}
	for f, v := range map[string]string{"-a": *spHost, "-r": *spRoom, "-n": *spName} {
		if v != "" {
			args = append(args, f, v)
		}
	}
	args = append(args, entries[0], "--", "--input-ipc-server="+filepath.Join(runDir, "shokod-mpv.sock"))
	spCmd = exec.Command("syncplay", args...)
	spCmd.Stdout, spCmd.Stderr = os.Stdout, os.Stderr
	log.Printf("exec syncplay %q", args)
	return spCmd.Start()
}

// mpv JSON IPC: one request at a time over a single connection, events are skipped.
func mpvGet(c net.Conn, prop string) (any, error) {
	if _, err := fmt.Fprintf(c, `{"command":["get_property",%q]}`+"\n", prop); err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(c)
	for sc.Scan() {
		var res struct {
			Data  any    `json:"data"`
			Error string `json:"error"`
			Event string `json:"event"`
		}
		if json.Unmarshal(sc.Bytes(), &res) != nil || res.Event != "" {
			continue
		}
		if res.Error != "success" {
			return nil, errors.New(res.Error)
		}
		return res.Data, nil
	}
	return nil, io.EOF
}

var streamRe = regexp.MustCompile(`/stream/(\d+)/`)

func markWatched(fileID string) {
	req, _ := http.NewRequest("POST", *shokoURL+"/api/v3/File/"+fileID+"/Watched/true", nil)
	req.Header.Set("apikey", *shokoKey)
	res, err := http.DefaultClient.Do(req)
	if err == nil {
		res.Body.Close()
		err = fmt.Errorf("%s", res.Status)
	}
	log.Printf("watched file %s: %v", fileID, err)
	var eps []episode
	if shokoGet("/api/v3/File/"+fileID+"/Episode", &eps) == nil && len(eps) > 0 {
		syncAnilist(eps[0].IDs.ParentSeries)
	}
}

var (
	anidbToAnilist map[int]int
	mapOnce        sync.Once
)

func anilistID(anidb int) int {
	mapOnce.Do(func() {
		cache := filepath.Join(env("XDG_CACHE_HOME", filepath.Join(os.Getenv("HOME"), ".cache")), "shokod-anime-list.json")
		st, err := os.Stat(cache)
		if err != nil || time.Since(st.ModTime()) > 7*24*time.Hour {
			res, err := http.Get("https://raw.githubusercontent.com/Fribb/anime-lists/master/anime-list-full.json")
			if err == nil && res.StatusCode == 200 {
				b, _ := io.ReadAll(res.Body)
				os.WriteFile(cache, b, 0o644)
			}
		}
		b, _ := os.ReadFile(cache)
		var list []struct {
			AniDB   int `json:"anidb_id"`
			AniList int `json:"anilist_id"`
		}
		json.Unmarshal(b, &list)
		anidbToAnilist = map[int]int{}
		for _, x := range list {
			anidbToAnilist[x.AniDB] = x.AniList
		}
	})
	return anidbToAnilist[anidb]
}

func anilistQuery(query string, vars map[string]any, out any) error {
	body, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	req, _ := http.NewRequest("POST", "https://graphql.anilist.co", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+*anilistToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var env struct {
		Data   json.RawMessage
		Errors []struct{ Message string }
	}
	if err := json.NewDecoder(res.Body).Decode(&env); err != nil {
		return err
	}
	if len(env.Errors) > 0 {
		return errors.New(env.Errors[0].Message)
	}
	return json.Unmarshal(env.Data, out)
}

// progress = highest watched ep number where every lower ep that has a file is watched.
func shokoProgress(eps []episode) int {
	n := 0
	for _, e := range eps {
		if e.Watched != nil {
			n = e.AniDB.EpisodeNumber
		} else if len(e.Files) > 0 {
			break
		}
	}
	return n
}

func syncAnilist(seriesID int) {
	if *anilistToken == "" {
		return
	}
	var series struct {
		Name string
		IDs  struct{ AniDB int }
	}
	eps, err := allEpisodes(seriesID)
	if err == nil {
		err = shokoGet(fmt.Sprintf("/api/v3/Series/%d", seriesID), &series)
	}
	if err != nil {
		log.Print("anilist: ", err)
		return
	}
	progress, alID := shokoProgress(eps), anilistID(series.IDs.AniDB)
	if progress == 0 || alID == 0 {
		log.Printf("anilist: %s skip (progress %d, anilist id %d)", series.Name, progress, alID)
		return
	}
	var m struct {
		Media struct {
			Episodes       int
			MediaListEntry *struct {
				Progress int
				Status   string
			}
		}
	}
	if err := anilistQuery(`query($id:Int){Media(id:$id){episodes mediaListEntry{progress status}}}`, map[string]any{"id": alID}, &m); err != nil {
		log.Print("anilist: ", err)
		return
	}
	if e := m.Media.MediaListEntry; e != nil && e.Progress >= progress {
		log.Printf("anilist: %s already at %d", series.Name, e.Progress)
		return
	}
	status := "CURRENT"
	if m.Media.Episodes > 0 && progress >= m.Media.Episodes {
		status = "COMPLETED"
	}
	err = anilistQuery(`mutation($id:Int,$p:Int,$s:MediaListStatus){SaveMediaListEntry(mediaId:$id,progress:$p,status:$s){id}}`,
		map[string]any{"id": alID, "p": progress, "s": status}, &struct{}{})
	log.Printf("anilist: %s -> %d %s: %v", series.Name, progress, status, err)
}

func watchMpv() {
	sock := filepath.Join(runDir, "shokod-mpv.sock")
	marked := map[string]bool{}
	for {
		time.Sleep(5 * time.Second)
		c, err := net.Dial("unix", sock)
		if err != nil {
			continue
		}
		for {
			path, err := mpvGet(c, "path")
			if err != nil {
				break
			}
			pct, _ := mpvGet(c, "percent-pos")
			m := streamRe.FindStringSubmatch(fmt.Sprint(path))
			if p, ok := pct.(float64); ok && m != nil && p >= *watchedAt && !marked[m[1]] {
				marked[m[1]] = true
				markWatched(m[1])
			}
			time.Sleep(5 * time.Second)
		}
		c.Close()
	}
}

func main() {
	flag.Parse()
	if *shokoKey == "" {
		log.Fatal("SHOKO_APIKEY required")
	}
	shoko, err := url.Parse(*shokoURL)
	if err != nil {
		log.Fatal(err)
	}
	if flag.Arg(0) == "anilist-sync" {
		var page struct {
			List []struct{ IDs struct{ ID int } }
		}
		if err := shokoGet("/api/v3/Series?pageSize=0", &page); err != nil {
			log.Fatal(err)
		}
		for _, s := range page.List {
			syncAnilist(s.IDs.ID)
		}
		return
	}

	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(shoko)
		r.Out.URL.Path = "/api/v3/File/" + r.In.PathValue("id") + "/Stream"
		r.Out.Header.Set("apikey", *shokoKey)
	}}

	cors := func(w http.ResponseWriter) {
		w.Header().Set("Access-Control-Allow-Origin", shoko.Scheme+"://"+shoko.Host)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /stream/{id}/{name}", proxy)
	mux.HandleFunc("GET /episodes", func(w http.ResponseWriter, r *http.Request) {
		cors(w)
		var id int
		if _, err := fmt.Sscan(r.FormValue("series"), &id); err != nil {
			http.Error(w, "series=<id> required", 400)
			return
		}
		eps, err := seriesEpisodes(id)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		type row struct {
			ID, Number int
			Name       string
			Watched    bool
		}
		rows := []row{}
		for _, e := range eps {
			rows = append(rows, row{e.IDs.ID, e.AniDB.EpisodeNumber, e.Name, e.Watched != nil})
		}
		json.NewEncoder(w).Encode(rows)
	})
	mux.HandleFunc("GET /syncplay", func(w http.ResponseWriter, r *http.Request) {
		cors(w)
		var id int
		if _, err := fmt.Sscan(r.FormValue("episode"), &id); err != nil {
			http.Error(w, "episode=<id> required", 400)
			return
		}
		entries, err := playlist(id, r.FormValue("mode"))
		if err == nil {
			err = launch(entries)
		}
		if err != nil {
			log.Print(err)
			http.Error(w, err.Error(), 500)
			return
		}
		fmt.Fprintf(w, "launched %d entries\n", len(entries))
	})

	go watchMpv()
	log.Printf("shokod on %s -> %s", *listen, *shokoURL)
	log.Fatal(http.ListenAndServe(*listen, mux))
}
