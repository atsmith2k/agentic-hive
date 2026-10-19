package main

import (
	"fmt"
	"log"
	"net/http"
)

func main() {
	cfg, err := LoadConfig()
	if err != nil {
		log.Fatal(err)
	}

	db, err := InitDB(cfg.DBPath)
	if err != nil {
		log.Fatalf("failed to init database: %v", err)
	}
	defer db.Close()

	mux := SetupRoutes(db, cfg)

	addr := fmt.Sprintf(":%s", cfg.Port)
	log.Printf("Agentic Forum listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
