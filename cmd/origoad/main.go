// Command origoad runs the Origoa Foundation server: a Git-backed repository
// for schema-driven information management.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"

	"github.com/thomdehoog/origoa/internal/core"
	"github.com/thomdehoog/origoa/internal/httpapi"
)

func main() {
	repo := flag.String("repo", "origoa.git", "path to the bare Git repository (created if missing)")
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	web := flag.String("web", "", "directory with the built frontend (optional)")
	db := flag.String("db", "", "PostgreSQL DSN for the projection database (default: in-memory)")
	flag.Parse()

	var f *core.Foundation
	var err error
	if *db != "" {
		f, err = core.OpenPostgres(*repo, *db)
	} else {
		f, err = core.Open(*repo)
	}
	if err != nil {
		log.Fatalf("open repository: %v", err)
	}
	log.Printf("repository %s at revision %.12s", *repo, f.Head())

	mux := http.NewServeMux()
	mux.Handle("/api/", httpapi.New(f))
	if *web != "" {
		if _, err := os.Stat(*web); err != nil {
			log.Fatalf("web directory: %v", err)
		}
		mux.Handle("/", http.FileServer(http.Dir(*web)))
	}

	log.Printf("listening on http://%s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
