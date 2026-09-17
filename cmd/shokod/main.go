package main

import (
	"flag"
	"log"
	"net/http"
	"os"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	listen := flag.String("listen", env("SHOKOD_LISTEN", "127.0.0.1:7373"), "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	log.Printf("shokod listening on %s", *listen)
	log.Fatal(http.ListenAndServe(*listen, mux))
}
