package main

import (
	"flag"
	"log"
	"net/http"

	"platform-api/internal/api"
	"platform-api/internal/audit"
	"platform-api/internal/cluster"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	keysPath := flag.String("keys", "keys.json", "path to the API keys JSON file")
	auditPath := flag.String("audit-log", "", "optional path to append audit entries as JSONL (in memory only if empty)")
	flag.Parse()

	keys, err := api.LoadKeys(*keysPath)
	if err != nil {
		log.Fatalf("load keys: %v", err)
	}
	log1, err := audit.Open(*auditPath)
	if err != nil {
		log.Fatalf("open audit log: %v", err)
	}
	defer log1.Close()

	srv := api.New(cluster.NewMemory(), keys, log1)
	log.Printf("platform-api listening on %s with %d keys", *addr, len(keys))
	log.Fatal(http.ListenAndServe(*addr, srv))
}
