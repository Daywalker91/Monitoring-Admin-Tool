package k8s

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReleases(t *testing.T) {
	body := `{"data":[{"id":"stable","latest":"v1.36.5+k3s1"},{"id":"latest","latest":"v1.37.1+k3s1"},
		{"id":"v1.35","latest":"v1.35.9+k3s1"},{"id":"testing","latest":"v1.38.0-rc1+k3s1"},
		{"id":"v1.34","latest":"<script>"}]}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/kaputt" {
			http.Error(w, "nein", http.StatusBadGateway)
			return
		}
		io.WriteString(w, body)
	}))
	defer ts.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	r := NewReleases(ts.URL, log)
	if ch, _ := r.Channels(); ch != nil {
		t.Fatal("vor dem abruf")
	}
	if err := r.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	ch, errText := r.Channels()
	if len(ch) != 3 || ch["stable"] != "v1.36.5+k3s1" || ch["v1.35"] != "v1.35.9+k3s1" || errText != "" {
		t.Fatalf("kanäle: %v %q", ch, errText)
	}

	bad := NewReleases(ts.URL+"/kaputt", log)
	if err := bad.Fetch(context.Background()); err == nil {
		t.Fatal("fehler nicht erkannt")
	}
	if ch, errText := bad.Channels(); ch != nil || errText == "" {
		t.Fatalf("nach fehler: %v %q", ch, errText)
	}
}
