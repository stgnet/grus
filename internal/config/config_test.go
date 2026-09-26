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
		if name == "grus.conf.example" && (len(c.Seed.Domains) != 1 || c.Seed.Domains[0] != "nfb.group") {
			t.Errorf("%s: domains %q", name, c.Seed.Domains)
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
	if c.Seed.Values["operators"] != "scott@stg.net" {
		t.Errorf("operators seed %q", c.Seed.Values["operators"])
	}
}

// A config from before the global level still loads: its domain, mail
// and AI lines become the seed, and worker lines are accepted but unused.
func TestOldKeysSeed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "old.conf")
	os.WriteFile(p, []byte(`node_id = n1
primary_domain = Old.Example
cluster_addr = :7946
advertise = 127.0.0.1:7946
bootstrap = true
tls_ca = a
tls_cert = b
tls_key = c
smtp_host = smtp.example.com
mail_from = hi@old.example
operator = A@x.com
operator = b@x.com
faq_hour = 3
worker = studio:7946
`), 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Seed.Domains) != 1 || c.Seed.Domains[0] != "old.example" || c.Seed.MailFrom != "hi@old.example" {
		t.Errorf("seed %+v", c.Seed)
	}
	v := c.Seed.Values
	if v["smtp_host"] != "smtp.example.com" || v["faq_hour"] != "3" || v["operators"] != "a@x.com\nb@x.com" {
		t.Errorf("seed values %q", v)
	}
	if len(c.Obsolete) != 1 || c.Obsolete[0] != "worker" {
		t.Errorf("obsolete %q", c.Obsolete)
	}
}

func TestUnknownKey(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.conf")
	os.WriteFile(p, []byte("node_id = n1\nprimry_domain = x.com\n"), 0o600)
	if _, err := Load(p); err == nil {
		t.Fatal("misspelled key accepted")
	}
}
