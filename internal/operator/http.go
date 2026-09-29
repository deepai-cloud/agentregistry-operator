package operator

import (
	"encoding/json"
	"io"
	"net/http"
)

func NewHandler(c Config) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	for _, path := range []string{"/sync", "/customize"} {
		mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
			defer r.Body.Close()
			d := json.NewDecoder(r.Body)
			var request Request
			if err := d.Decode(&request); err != nil {
				http.Error(w, "invalid webhook JSON", 400)
				return
			}
			if err := d.Decode(new(any)); err != io.EOF {
				http.Error(w, "expected one JSON object", 400)
				return
			}
			var response any
			var err error
			if r.URL.Path == "/sync" {
				response, err = Sync(request, c)
			} else {
				response, err = Customize(request.Parent)
			}
			if err != nil {
				http.Error(w, err.Error(), 422)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(response)
		})
	}
	return mux
}
