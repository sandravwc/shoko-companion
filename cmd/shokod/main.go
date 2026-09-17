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
	listen    = flag.String("listen", env("SHOKOD_LISTEN", "127.0.0.1:7373"), "listen address")
	shokoURL  = flag.String("shoko", env("SHOKO_URL", "http://127.0.0.1:8111"), "Shoko Server URL")
	shokoKey  = flag.String("shoko-key", env("SHOKO_APIKEY", ""), "Shoko API key")
	spHost    = flag.String("sp-host", env("SYNCPLAY_HOST", ""), "syncplay host:port (default: syncplay.ini)")
	spRoom    = flag.String("sp-room", env("SYNCPLAY_ROOM", ""), "syncplay room (default: syncplay.ini)")
	spName    = flag.String("sp-name", env("SYNCPLAY_NAME", ""), "syncplay username (default: syncplay.ini)")
	watchedAt = flag.Float64("watched-at", 85, "mark watched in Shoko at this percent-pos")
	runDir    = env("XDG_RUNTIME_DIR", os.TempDir())
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

func seriesEpisodes(seriesID int) ([]episode, error) {
	var page struct{ List []episode }
	err := shokoGet(fmt.Sprintf("/api/v3/Series/%d/Episode?pageSize=0&includeFiles=true&includeDataFrom=AniDB", seriesID), &page)
	var eps []episode
	for _, e := range page.List {
		if e.AniDB.Type == "Episode" && len(e.Files) > 0 {
			eps = append(eps, e)
		}
	}
	sort.Slice(eps, func(i, j int) bool { return eps[i].AniDB.EpisodeNumber < eps[j].AniDB.EpisodeNumber })
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
		for i, e := range eps {
			if e.IDs.ID == episodeID {
				eps = append(eps[i:], eps[:i]...)
				break
			}
		}
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
