package main

import (
	"context"
	"log"
	"net/http"
	"os"
)

func main() {
	ctx := context.Background()

	w, err := wireUp(ctx)
	defer func() {
		for _, f := range w.CleanUp {
			_ = f()
		}
	}()
	if err != nil {
		log.Println(err)
		return
	}

	mux := http.NewServeMux()
	routes(mux, w.App)

	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	err = http.ListenAndServe(addr, mux)
	if err != nil {
		log.Println(err)
		return
	}
}
