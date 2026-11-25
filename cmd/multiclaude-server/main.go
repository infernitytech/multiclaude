package main

import (
	"flag"
	"log"
	"os"

	"multiclaude/internal/server"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	addr := flag.String("addr", env("MC_ADDR", ":8080"), "listen address")
	data := flag.String("data", env("MC_DATA", "./data"), "data directory (repos, LFS objects, database)")
	dl := flag.String("downloads", env("MC_DOWNLOADS", ""), "directory with prebuilt runner binaries served at /download/")
	flag.Parse()
	s, err := server.New(*data, *addr, *dl)
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(s.Run())
}
