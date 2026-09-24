package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The example configs must stay loadable: they're what people copy.
func TestExamplesLoad(t *testing.T) {
	for _, name := range []string{"grus.conf.example", "studio.conf.example"} {
		c, err := Load(filepath.Join("..", "..", "deploy", name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.PrimaryDomain != "nfb.group" {
			t.Errorf("%s: primary %q", name, c.PrimaryDomain)
		}
	}
	c, _ := Load("../../deploy/grus.conf.example")
	if !c.Voter || !c.Bootstrap {
		t.Errorf("the VPS example should bootstrap as a voter: %+v", c)
	}
	s, _ := Load("../../deploy/studio.conf.example")
	if !s.Full || s.Voter || len(s.Join) != 1 || s.Join[0] != "vps1.nfb.group:7946" {
		t.Errorf("studio: full %v voter %v join %v", s.Full, s.Voter, s.Join)
	}
	if !c.IsOperator("Scott@stg.net") {
		t.Error("operator match should ignore case")
	}
}

func TestUnknownKey(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.conf")
	os.WriteFile(p, []byte("node_id = n1\nprimry_domain = x.com\n"), 0o600)
	if _, err := Load(p); err == nil {
		t.Fatal("misspelled key accepted")
	}
}
