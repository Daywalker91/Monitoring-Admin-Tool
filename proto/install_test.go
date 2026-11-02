package proto

import (
	"strings"
	"testing"
)

func TestValidPackageName(t *testing.T) {
	for _, s := range []string{"nano", "g++", "libstdc++6", "python3.12", "libc6:arm64", "NetworkManager-libnm", "Microsoft.PowerShell", "a"} {
		if !ValidPackageName(s) {
			t.Errorf("%q abgelehnt", s)
		}
	}
	for _, s := range []string{
		"", "-oAPT::Update", "--force", "foo;rm", "a b", "nano-", "nano.", "nano:", "foo=1.0", "foo/stable", "../foo",
		"./foo.deb", "foo.deb", "foo.RPM", "x.apk", "setup.msi", "setup.exe", "foo*", "fo?", "[a]", "!nano", "@group", "~foo",
		"foo\n", "foo\x00", "päckchen", "ｎａｎｏ", "$(id)", "`id`", "a|b", "a&b", "a>b", strings.Repeat("a", 101),
	} {
		if ValidPackageName(s) {
			t.Errorf("%q angenommen", s)
		}
	}
}

func TestValidUserName(t *testing.T) {
	for _, s := range []string{"hans", "a", "deploy-1", "web_user", "h" + strings.Repeat("a", 31)} {
		if !ValidUserName(s) {
			t.Errorf("%q abgelehnt", s)
		}
	}
	for _, s := range []string{
		"", "root", "daemon", "nobody", "administrator", "system", "sudo", "wheel", "systemd-network", "_apt", "Hans", "1abc", "-x",
		"--help", "a b", "a/b", `a\b`, "a:b", "a;b", "a@b", "a.b", "a$", "hänsel", "foo\n", strings.Repeat("a", 33),
	} {
		if ValidUserName(s) {
			t.Errorf("%q angenommen", s)
		}
	}
}

func TestInstallJobValidate(t *testing.T) {
	for _, c := range []struct {
		action string
		params map[string]string
		ok     bool
	}{
		{ActionPackageInstall, map[string]string{"name": "nano"}, true},
		{ActionPackageInstall, map[string]string{"name": "nano-"}, false},
		{ActionPackageInstall, nil, false},
		{ActionPackageInstall, map[string]string{"name": "nano", "version": "1"}, false},
		{ActionUserCreate, map[string]string{"name": "hans"}, true},
		{ActionUserCreate, map[string]string{"name": "hans", "sudo": "true"}, true},
		{ActionUserCreate, map[string]string{"name": "hans", "sudo": "ja"}, false},
		{ActionUserCreate, map[string]string{"name": "root"}, false},
		{ActionUserCreate, map[string]string{"sudo": "true"}, false},
		{ActionUserCreate, map[string]string{"name": "hans", "password": "x"}, false},
	} {
		j := JobRequest{JobID: "j1", Action: c.action, Params: c.params}
		if err := j.Validate(); (err == nil) != c.ok {
			t.Errorf("%s %v: err=%v, erwartet ok=%v", c.action, c.params, err, c.ok)
		}
	}
}
