package main

import (
    "flag"
    "fmt"
    "log"
    "net/http"
    "sort"
)

func echoHandler(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "text/plain; charset=utf-8")
    fmt.Fprintf(w, "method=%s\n", r.Method)
    fmt.Fprintf(w, "path=%s\n", r.URL.RequestURI())
    fmt.Fprintf(w, "remote=%s\n\n", r.RemoteAddr)

    // Print headers in sorted order for stable output
    keys := make([]string, 0, len(r.Header))
    for k := range r.Header { keys = append(keys, k) }
    sort.Strings(keys)
    for _, k := range keys {
        for _, v := range r.Header[k] {
            fmt.Fprintf(w, "%s: %s\n", k, v)
        }
    }
}

func main() {
    addr := flag.String("addr", "127.0.0.1:9004", "listen address (e.g., 127.0.0.1:9004 or [::1]:9004)")
    flag.Parse()
    http.HandleFunc("/", echoHandler)
    log.Printf("echo server listening on %s", *addr)
    log.Fatal(http.ListenAndServe(*addr, nil))
}

