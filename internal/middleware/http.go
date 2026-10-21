package middleware

import (
	"log"
	"net/http"
	"runtime/debug"
	"strings"
)

// SecurityHeaders marks every response as a non-cacheable, non-embeddable API
// reply. The CSP is deliberately empty-handed: this service never serves
// documents, so there is nothing a browser should be willing to run from it.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		h.Set("Cross-Origin-Resource-Policy", "same-site")
		next.ServeHTTP(w, r)
	})
}

// CORS lets only the listed browser origins call the API. It sends no
// Allow-Credentials: the API has no cookie sessions, so cross-origin pages
// have no reason to attach any.
// UserMessageHeader marks a response whose body was written for the person
// using the site, so the frontend may show it as it is. Error pages from a proxy
// or a platform never carry it, which is how the two are told apart.
const UserMessageHeader = "X-User-Message"

// UserError writes a plain-text error whose wording is meant to be read by a
// parent or customer (a full sentence saying what happened and what to do).
func UserError(w http.ResponseWriter, message string, status int) {
	w.Header().Set(UserMessageHeader, "1")
	http.Error(w, message, status)
}

func CORS(allowedOrigins []string, next http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		allowed[strings.TrimRight(o, "/")] = struct{}{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		_, ok := allowed[origin]
		w.Header().Add("Vary", "Origin")

		if ok {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Expose-Headers", UserMessageHeader)
			w.Header().Set("Access-Control-Max-Age", "600")
		}

		if r.Method == http.MethodOptions {
			if ok {
				w.WriteHeader(http.StatusNoContent)
			} else {
				w.WriteHeader(http.StatusForbidden)
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Recover turns a handler panic into a 500 instead of a dropped connection,
// and keeps the stack in the server log rather than the response.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				log.Printf("[PANIC] %s %s: %v\n%s", r.Method, r.URL.Path, v, debug.Stack())
				http.Error(w, "Internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
