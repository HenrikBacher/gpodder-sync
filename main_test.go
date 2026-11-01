package main

import (
	"fmt"
	"testing"
)

// parsePrefixes decides which peers may set the user header, so a mistake
// here lets other containers impersonate users.
func TestParsePrefixes(t *testing.T) {
	cases := []struct {
		in      string
		want    string // fmt of the result
		wantErr bool
	}{
		{in: "", want: "[]"},
		{in: "172.30.99.10", want: "[172.30.99.10/32]"},
		{in: "fd00::10", want: "[fd00::10/128]"},
		{in: "10.0.0.7/8", want: "[10.0.0.0/8]"}, // masked
		{in: " 172.30.99.10 , 127.0.0.1,", want: "[172.30.99.10/32 127.0.0.1/32]"},
		{in: "172.30.99", wantErr: true},
		{in: "172.30.99.10/33", wantErr: true},
		{in: "caddy", wantErr: true},
	}
	for _, c := range cases {
		got, err := parsePrefixes(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parsePrefixes(%q) = %v, want error", c.in, got)
			}
			continue
		}
		if err != nil || fmt.Sprint(got) != c.want {
			t.Errorf("parsePrefixes(%q) = %v, %v; want %s", c.in, got, err, c.want)
		}
	}
}
