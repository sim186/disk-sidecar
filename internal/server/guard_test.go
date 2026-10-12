package server

import "testing"

func TestGuardUnix(t *testing.T) {
	g := guardEnv{home: "/Users/me", external: []string{"/Volumes", "/media", "/mnt", "/run/media"}}
	ok := []string{"/Users/me/Documents/a.txt", "/Users/me/Pictures", "/Volumes/Backup/anything", "/media/me/Disk/x", "/Users/other/Desktop/x"}
	bad := []string{"/", "/System/Library/x", "/usr/bin/ls", "/etc/hosts", "/Applications/App.app", "/Library/Preferences", "/private/var/x",
		"/Users", "/Users/me", "/Users/me/Library/Mail", "/Users/me/.ssh/id_rsa", "/Users/other", "/opt/x", "/Volumes"}
	for _, p := range ok {
		if err := g.check(p); err != nil {
			t.Errorf("%s refused: %v", p, err)
		}
	}
	for _, p := range bad {
		if g.check(p) == nil {
			t.Errorf("%s allowed", p)
		}
	}
}

func TestGuardWindows(t *testing.T) {
	g := guardEnv{windows: true, home: `C:\Users\me`, sysDrive: "C:"}
	ok := []string{`C:\Users\me\Documents\a.txt`, `c:\users\me\Pictures`, `D:\anything\x`, `E:\Windows\x`}
	bad := []string{`C:\Windows\System32`, `C:\Program Files\App`, `C:\ProgramData`, `C:\Users`, `C:\Users\me`, `C:\Users\me\AppData\Local`, `C:\Users\Other`, `C:\`}
	for _, p := range ok {
		if err := g.check(p); err != nil {
			t.Errorf("%s refused: %v", p, err)
		}
	}
	for _, p := range bad {
		if g.check(p) == nil {
			t.Errorf("%s allowed", p)
		}
	}
}
