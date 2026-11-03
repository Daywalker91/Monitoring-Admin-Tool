package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/auth"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/playbook"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

func TestPlaybookHTTP(t *testing.T) {
	s, _, _ := newPlaybookServer(t, filepath.Join(t.TempDir(), "mat.db"), threeHosts(), map[string]string{
		"p.yaml": fmt.Sprintf(patchBook, "serial", 0, "stop"),
		"k.yaml": "name: kaputt\ntargets: [n1]\nsteps: [{id: a, action: shell.run}]\n",
		"z.yaml": "name: ziel\ntargets: [weg]\nsteps: [{id: a, wait: 1s}]\n"})
	s.auth = auth.New(s.store)
	hash, _ := auth.HashPassword(testPassword)
	s.store.SetUser(context.Background(), "admin", hash, time.Now())
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	if code := status(t, newClient(t), http.MethodGet, ts.URL+"/api/playbooks", ""); code != http.StatusUnauthorized {
		t.Fatalf("ohne anmeldung: %d", code)
	}
	c := login(t, ts)

	var list playbookList
	if code := getJSON(t, c, ts.URL+"/api/playbooks", &list); code != http.StatusOK || !list.Enabled || len(list.Files) != 3 {
		t.Fatalf("%d %+v", code, list)
	}
	if list.Files[0].File.File != "k.yaml" || !strings.Contains(list.Files[0].Err, "unbekannte aktion") || list.Files[1].Err != "" ||
		list.Files[2].Err != "unbekannte ziele: weg" {
		t.Errorf("dateien: %+v", list.Files)
	}

	var preview struct {
		Hosts    []previewHost `json:"hosts"`
		Blockers []string      `json:"blockers"`
	}
	if code := getJSON(t, c, ts.URL+"/api/playbooks/patchen/preview", &preview); code != http.StatusOK || len(preview.Hosts) != 3 {
		t.Fatalf("vorschau: %d %+v", code, preview)
	}
	// Testhosts melden keine Fähigkeiten und sind nicht verbunden.
	if h := preview.Hosts[0]; h.Online || len(h.Steps) != 2 || h.Steps[0] != (previewStep{previewFail, "fähigkeit packages fehlt"}) ||
		len(preview.Blockers) != 0 {
		t.Errorf("probelauf: %+v", preview)
	}
	if code := getJSON(t, c, ts.URL+"/api/playbooks/kaputt/preview", &struct{}{}); code != http.StatusConflict {
		t.Errorf("vorschau fehlerhaft: %d", code)
	}

	if code := status(t, c, http.MethodPost, ts.URL+"/api/playbooks/patchen/run", "https://evil.example"); code != http.StatusForbidden {
		t.Errorf("fremde herkunft: %d", code)
	}
	if code := status(t, c, http.MethodPost, ts.URL+"/api/playbooks/fehlt/run", ts.URL); code != http.StatusNotFound {
		t.Errorf("unbekannt: %d", code)
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/playbooks/patchen/run", nil)
	req.Header.Set("Origin", ts.URL)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var run store.PlaybookRun
	json.NewDecoder(resp.Body).Decode(&run)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || run.User != "admin" {
		t.Fatalf("start: %d %+v", resp.StatusCode, run)
	}
	waitRun(t, s, run.ID)

	var view runView
	if code := getJSON(t, c, ts.URL+"/api/playbook-runs/"+run.ID, &view); code != http.StatusOK ||
		view.Status != store.RunOK || len(view.Steps) != 6 || view.Playbook == nil || view.HostNames["n1"] != "n1" {
		t.Fatalf("lauf: %d %+v", code, view)
	}
	var runs []store.PlaybookRun
	if code := getJSON(t, c, ts.URL+"/api/playbook-runs", &runs); code != http.StatusOK || len(runs) != 1 {
		t.Fatalf("verlauf: %d %+v", code, runs)
	}
	if code := status(t, c, http.MethodPost, ts.URL+"/api/playbook-runs/"+run.ID+"/cancel", ts.URL); code != http.StatusNotFound {
		t.Errorf("beendeten lauf abbrechen: %d", code)
	}
	if code := status(t, c, http.MethodPost, ts.URL+"/api/playbooks/reload", ts.URL); code != http.StatusOK {
		t.Errorf("neu laden: %d", code)
	}
}

func TestPlaybooksDisabled(t *testing.T) {
	ts := newTestServer(t)
	c := login(t, ts)
	var list playbookList
	if code := getJSON(t, c, ts.URL+"/api/playbooks", &list); code != http.StatusOK || list.Enabled {
		t.Fatalf("%d %+v", code, list)
	}
	if code := status(t, c, http.MethodPost, ts.URL+"/api/playbooks/x/run", ts.URL); code != http.StatusNotFound {
		t.Errorf("start ohne playbooks: %d", code)
	}
}

// TestPreviewStep prüft die Einschätzung einzelner Schritte im Probelauf.
func TestPreviewStep(t *testing.T) {
	s, _, _ := newPlaybookServer(t, filepath.Join(t.TempDir(), "mat.db"), threeHosts(), nil)
	ctx := context.Background()
	h := store.Host{ID: "n1", Capabilities: []proto.Capability{proto.CapPackages, proto.CapCommands},
		Commands: []proto.CommandInfo{{Name: "amp-update"}}, Updates: &store.UpdateSummary{Upgradable: 4, Security: 1}}
	for _, c := range []struct {
		step   playbook.Step
		online bool
		want   previewStep
	}{
		{playbook.Step{Wait: playbook.Duration(5 * time.Minute)}, true, previewStep{previewInfo, "wartet 5m0s"}},
		{playbook.Step{Action: proto.ActionPackagesUpgrade}, true, previewStep{previewInfo, "4 updates ausstehend (1 sicherheit)"}},
		{playbook.Step{Action: proto.ActionPackagesUpgrade}, false, previewStep{previewWarn, "host ist offline"}},
		{playbook.Step{Action: proto.ActionCommandRun, Params: map[string]string{"name": "amp-update"}}, true, previewStep{previewOK, "bereit"}},
		{playbook.Step{Action: proto.ActionCommandRun, Params: map[string]string{"name": "x"}}, true, previewStep{previewFail, "befehl x nicht freigegeben"}},
		{playbook.Step{Action: proto.ActionReboot}, true, previewStep{previewFail, "fähigkeit reboot fehlt"}},
		{playbook.Step{Action: playbook.ActionDrainReboot}, true, previewStep{previewFail, "drain nicht verfügbar (clusterrole mat-server-drain fehlt)"}},
		{playbook.Step{Gate: playbook.GateHostHealthy}, true, previewStep{previewWarn, "jetzt nicht erfüllt: host nicht verbunden"}},
		{playbook.Step{Gate: playbook.GateClusterHealthy}, true, previewStep{previewFail, "k3s-ansicht ist abgeschaltet"}},
	} {
		if got := s.previewStep(ctx, c.step, h, c.online); got != c.want {
			t.Errorf("%+v: %+v, erwartet %+v", c.step, got, c.want)
		}
	}
}
