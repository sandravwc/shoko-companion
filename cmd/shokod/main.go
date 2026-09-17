package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

var (
	listen   = flag.String("listen", env("SHOKOD_LISTEN", "127.0.0.1:7373"), "listen address")
	shokoURL = flag.String("shoko", env("SHOKO_URL", "http://127.0.0.1:8111"), "Shoko Server URL")
	shokoKey = flag.String("shoko-key", env("SHOKO_APIKEY", ""), "Shoko API key")
	spHost   = flag.String("sp-host", env("SYNCPLAY_HOST", ""), "syncplay host:port (default: syncplay.ini)")
	spRoom   = flag.String("sp-room", env("SYNCPLAY_ROOM", ""), "syncplay room (default: syncplay.ini)")
	spName   = flag.String("sp-name", env("SYNCPLAY_NAME", ""), "syncplay username (default: syncplay.ini)")
	runDir   = env("XDG_RUNTIME_DIR", os.TempDir())
)

type episode struct {
	IDs   struct{ ID, ParentSeries int }
	AniDB struct {
		Type          string
		EpisodeNumber int
	}
	Files []struct{ ID int }
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
		var page struct{ List []episode }
		if err := shokoGet(fmt.Sprintf("/api/v3/Series/%d/Episode?pageSize=0&includeFiles=true&includeDataFrom=AniDB", ep.IDs.ParentSeries), &page); err != nil {
			return nil, err
		}
		eps = eps[:0]
		for _, e := range page.List {
			if e.AniDB.Type == "Episode" && len(e.Files) > 0 {
				eps = append(eps, e)
			}
		}
		sort.Slice(eps, func(i, j int) bool { return eps[i].AniDB.EpisodeNumber < eps[j].AniDB.EpisodeNumber })
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

	mux := http.NewServeMux()
	mux.Handle("GET /stream/{id}/{name}", proxy)
	mux.HandleFunc("GET /syncplay", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", shoko.Scheme+"://"+shoko.Host)
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

	log.Printf("shokod on %s -> %s", *listen, *shokoURL)
	log.Fatal(http.ListenAndServe(*listen, mux))
}
