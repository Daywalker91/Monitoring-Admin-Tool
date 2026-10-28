package release

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSignVerify(t *testing.T) {
	privText, pubText, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	priv, err := ParsePrivateKey(privText)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ParsePublicKey(pubText)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	for name, body := range map[string]string{
		"mat-agent-linux-amd64":       "a",
		"mat-agent-linux-armv7":       "bb",
		"mat-agent-windows-amd64.exe": "ccc",
		"install-agent.sh":            "x", // kein Agent-Binary, gehört nicht ins Manifest
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := BuildManifest("v0.5.0", dir)
	if err != nil {
		t.Fatal(err)
	}
	sig := Sign(priv, manifest)

	m, err := VerifyWith(pub, manifest, sig)
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "v0.5.0" || len(m.Files) != 3 {
		t.Fatalf("manifest = %+v", m)
	}
	f, ok := m.For("linux", "arm")
	if !ok || f.Name != "mat-agent-linux-armv7" || f.Size != 2 {
		t.Fatalf("arm-eintrag = %+v, %v", f, ok)
	}
	if err := CheckFile(filepath.Join(dir, f.Name), f); err != nil {
		t.Fatal(err)
	}
	if err := CheckFile(filepath.Join(dir, "mat-agent-linux-amd64"), f); err == nil {
		t.Fatal("falsche datei akzeptiert")
	}

	// Verändertes Manifest oder fremder Schlüssel fallen durch.
	if _, err := VerifyWith(pub, append([]byte(" "), manifest...), sig); err == nil {
		t.Fatal("verändertes manifest akzeptiert")
	}
	_, otherPub, _ := GenerateKey()
	other, _ := ParsePublicKey(otherPub)
	if _, err := VerifyWith(other, manifest, sig); err == nil {
		t.Fatal("fremder schlüssel akzeptiert")
	}

	// Ohne eingebauten Schlüssel ist Self-Update aus.
	if _, err := Verify(manifest, sig); err != ErrNoKey {
		t.Fatalf("Verify ohne schlüssel: %v", err)
	}
	publicKey = pubText
	defer func() { publicKey = "" }()
	if _, err := Verify(manifest, sig); err != nil {
		t.Fatalf("Verify mit eingebautem schlüssel: %v", err)
	}
}

func TestManifestValidate(t *testing.T) {
	good := File{Name: "mat-agent-linux-amd64", OS: "linux", Arch: "amd64", Size: 1,
		SHA256: "0000000000000000000000000000000000000000000000000000000000000000"}
	bad := []Manifest{
		{Version: "v1.0.0-rc1", Files: []File{good}},
		{Version: "v1.0.0"},
		{Version: "v1.0.0", Files: []File{good, good}},
		{Version: "v1.0.0", Files: []File{{Name: "../evil", OS: "linux", Arch: "amd64", Size: 1, SHA256: good.SHA256}}},
		{Version: "v1.0.0", Files: []File{{Name: good.Name, OS: "linux", Arch: "amd64", Size: MaxBinaryBytes + 1, SHA256: good.SHA256}}},
	}
	for i, m := range bad {
		if err := m.Validate(); err == nil {
			data, _ := json.Marshal(m)
			t.Errorf("%d: ungültiges manifest akzeptiert: %s", i, data)
		}
	}
	ok := Manifest{Version: "0.5.0", Files: []File{good}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestNewer(t *testing.T) {
	cases := []struct {
		cand, cur string
		want      bool
	}{
		{"v0.5.1", "v0.5.0", true},
		{"v0.5.0", "v0.5.0", false},
		{"v0.4.9", "v0.5.0", false},
		{"v0.10.0", "v0.9.3", true},
		{"0.5.1", "v0.5.0", true},
		{"v0.5.0", "v0.5.0-3-gabc1234-dirty", false},
		{"v0.5.1", "v0.5.0-3-gabc1234", true},
		{"v0.5.0", "dev", true},
		{"v0.5.0", "a158335", true},
		{"v0.6.0-rc1", "v0.5.0", false},
		{"kaputt", "v0.5.0", false},
	}
	for _, c := range cases {
		if got := Newer(c.cand, c.cur); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, will %v", c.cand, c.cur, got, c.want)
		}
	}
}

func TestBinaryName(t *testing.T) {
	for _, c := range [][3]string{
		{"linux", "amd64", "mat-agent-linux-amd64"},
		{"linux", "arm", "mat-agent-linux-armv7"},
		{"windows", "amd64", "mat-agent-windows-amd64.exe"},
	} {
		if got := BinaryName(c[0], c[1]); got != c[2] {
			t.Errorf("BinaryName(%s, %s) = %s", c[0], c[1], got)
		}
		if goos, goarch, ok := platformOf(c[2]); !ok || goos != c[0] || goarch != c[1] {
			t.Errorf("platformOf(%s) = %s %s %v", c[2], goos, goarch, ok)
		}
	}
	for _, n := range []string{"mat-agent-linux-arm", "mat-agent-linux-amd64.exe", "mat-agent", "install-agent.sh"} {
		if _, _, ok := platformOf(n); ok {
			t.Errorf("platformOf(%s) akzeptiert", n)
		}
	}
}
