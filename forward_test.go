package main

import "testing"

func TestParseForwardSpec(t *testing.T) {
	cases := []struct {
		spec, flag, value string
		wantErr           bool
	}{
		{"8080", "-L", "8080:localhost:8080", false},
		{"8080:80", "-L", "8080:localhost:80", false},
		{"1:80", "-L", "1:localhost:80", false},
		{"5433:db.internal:5432", "-L", "5433:db.internal:5432", false},
		{"0.0.0.0:8080:web:80", "-L", "0.0.0.0:8080:web:80", false},
		{"L:8080:80", "-L", "8080:localhost:80", false},
		{"R:9000:3000", "-R", "9000:localhost:3000", false},
		{"r:9000:web:3000", "-R", "9000:web:3000", false},
		{"D:1080", "-D", "1080", false},
		{"d:127.0.0.1:1080", "-D", "127.0.0.1:1080", false},
		{"abc", "", "", true},
		{"70000", "", "", true},
		{"8080::80", "", "", true},
		{"X:80", "", "", true},
		{"1:2:3:4:5", "", "", true},
	}
	for _, c := range cases {
		f, err := parseForwardSpec(c.spec, "prod")
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: expected error, got %+v", c.spec, f)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.spec, err)
			continue
		}
		if f.flag != c.flag || f.value != c.value {
			t.Errorf("%q: got %s %s, want %s %s", c.spec, f.flag, f.value, c.flag, c.value)
		}
	}
}
