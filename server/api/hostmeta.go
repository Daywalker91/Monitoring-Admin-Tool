package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxMetaLen begrenzt Anzeigename und Gruppe (Zeichen).
const maxMetaLen = 64

// hostMeta sind die in der GUI änderbaren Angaben eines Hosts.
type hostMeta struct {
	DisplayName string `json:"display_name"`
	Group       string `json:"group"`
}

// validMeta bereinigt v (Leerraum am Rand) und prüft Länge und Zeichen.
func validMeta(v string) (string, bool) {
	v = strings.TrimSpace(v)
	ok := utf8.ValidString(v) && utf8.RuneCountInString(v) <= maxMetaLen &&
		strings.IndexFunc(v, unicode.IsControl) < 0
	return v, ok
}

// handleHostMeta setzt Anzeigename und Gruppe eines Hosts. Leere Werte setzen
// auf den Hostnamen bzw. „ohne Gruppe“ zurück.
func (s *Server) handleHostMeta(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "fremde herkunft", http.StatusForbidden)
		return
	}
	var m hostMeta
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&m); err != nil {
		http.Error(w, "ungültige anfrage", http.StatusBadRequest)
		return
	}
	var ok1, ok2 bool
	m.DisplayName, ok1 = validMeta(m.DisplayName)
	m.Group, ok2 = validMeta(m.Group)
	if !ok1 || !ok2 {
		http.Error(w, "name und gruppe: höchstens 64 zeichen, keine steuerzeichen", http.StatusBadRequest)
		return
	}
	id := r.PathValue("id")
	found, err := s.store.SetHostMeta(r.Context(), id, m.DisplayName, m.Group)
	if err != nil {
		s.log.Error("host-angaben speichern", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "host nicht gefunden", http.StatusNotFound)
		return
	}
	s.setName(id, m.DisplayName)
	s.log.Info("host-angaben geändert", "agent_id", id, "name", m.DisplayName, "group", m.Group)
	writeJSON(w, m)
}
