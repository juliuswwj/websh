package main

import "testing"

func TestPlanRename(t *testing.T) {
	isRemote := func(id string) bool { return id == "server" || id == "gpu" }

	cases := []struct {
		name        string
		target, raw string
		wantErr     bool
		remote      bool
		newName     string // local proxy / local session name
		oldPrefix   string
		newPrefix   string
	}{
		{name: "local ok", target: "main", raw: "work", newName: "work"},
		{name: "local trims", target: "main", raw: "  work  ", newName: "work"},
		{name: "local rejects @", target: "main", raw: "fake@server", wantErr: true},
		{name: "local rejects empty", target: "sh1", raw: "  ", wantErr: true},
		{name: "remote prefix only", target: "1@server", raw: "work", remote: true, oldPrefix: "1", newPrefix: "work", newName: "work@server"},
		{name: "remote keeps id even if client adds suffix", target: "1@server", raw: "work@bogus", remote: true, oldPrefix: "1", newPrefix: "work", newName: "work@server"},
		{name: "remote rejects @ in prefix", target: "1@server", raw: "a@b@server", wantErr: true},
		{name: "remote rejects empty prefix", target: "1@server", raw: "@server", wantErr: true},
		{name: "unknown @suffix is local (and @ rejected)", target: "x@notremote", raw: "y@notremote", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := planRename(c.target, c.raw, isRemote)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got plan %+v", p)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if p.remote != c.remote || p.newName != c.newName {
				t.Fatalf("plan = %+v, want remote=%v newName=%q", p, c.remote, c.newName)
			}
			if c.remote && (p.oldPrefix != c.oldPrefix || p.newPrefix != c.newPrefix || p.remoteID != "server") {
				t.Fatalf("remote plan = %+v, want old=%q new=%q id=server", p, c.oldPrefix, c.newPrefix)
			}
		})
	}
}
