package main

import (
	"flag"
	"net/http"
	"os"
	"strconv"
)

var response = "lrt/test: OK"

var overridePort = flag.Int("override-port", 0, "")

func main() {

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(response))
	})
	port := os.Getenv("PORT")
	if *overridePort != 0 {
		port = strconv.Itoa(*overridePort)
	}

	http.ListenAndServe("localhost:"+port, nil)
}
