package main

import (
	"fmt"
	"log/slog"
	"net/http"
)

const dataSize = 128 * 1024 * 1024

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		slog.Info("New session", slog.String("from", r.RemoteAddr))
		defer slog.Info("Session finished", slog.String("from", r.RemoteAddr))

		w.Header().Set("Content-Length", fmt.Sprintf("%d", dataSize))
		for i := 0; i < dataSize; i++ {
			if _, err := fmt.Fprint(w, "A"); err != nil {
				return
			}
		}
	})

	if err := http.ListenAndServe(":8080", nil); err != nil {
		panic(err)
	}
}
