package main

import (
	"strings"
	"testing"
)

func goodPool() Pool {
	return Pool{Name: "ubuntu", Runtime: "ubuntu", Count: 2, DockerSocket: true, GracefulStopSeconds: 900}
}

func TestPool_EffectiveImage(t *testing.T) {
	cases := []struct {
		runtime, image, want string
	}{
		{"ubuntu", "", RunnerImageRepo + ":ubuntu"},
		{"debian", "", RunnerImageRepo + ":debian"},
		{"minideb", "", RunnerImageRepo + ":minideb"},
		{"ubuntu", RunnerImageRepo + ":ubuntu-2.336.0", RunnerImageRepo + ":ubuntu-2.336.0"},
		{"custom", "registry.example.com/runner:1", "registry.example.com/runner:1"},
		{"custom", "", ""},
	}
	for _, c := range cases {
		p := Pool{Runtime: c.runtime, Image: c.image}
		if got := p.EffectiveImage(); got != c.want {
			t.Errorf("runtime=%q image=%q: got %q, want %q", c.runtime, c.image, got, c.want)
		}
	}
	if DefaultRunnerImage != RunnerImageRepo+":ubuntu" || LegacyRunnerImage != RunnerImageRepo+":latest" {
		t.Errorf("image constants: %q %q", DefaultRunnerImage, LegacyRunnerImage)
	}
}

func TestPool_Validate(t *testing.T) {
	if err := goodPool().Validate(); err != nil {
		t.Fatalf("good pool must validate: %v", err)
	}
	bad := map[string]func(*Pool){
		"empty name":      func(p *Pool) { p.Name = "" },
		"uppercase name":  func(p *Pool) { p.Name = "Ubuntu" },
		"long name":       func(p *Pool) { p.Name = strings.Repeat("a", 25) },
		"bad runtime":     func(p *Pool) { p.Runtime = "Arch Linux" },
		"empty runtime":   func(p *Pool) { p.Runtime = "" },
		"custom no image": func(p *Pool) { p.Runtime = "custom"; p.Image = "" },
		"negative count":  func(p *Pool) { p.Count = -1 },
		"huge count":      func(p *Pool) { p.Count = maxRunnerCount + 1 },
		"negative stop":   func(p *Pool) { p.GracefulStopSeconds = -1 },
		"relative work":   func(p *Pool) { p.WorkBase = "srv/gha" },
		"env no equals":   func(p *Pool) { p.ExtraEnv = "NOEQUALS" },
		"env reserved":    func(p *Pool) { p.ExtraEnv = "RUNNER_TOKEN=x" },
	}
	for name, mutate := range bad {
		p := goodPool()
		mutate(&p)
		if err := p.Validate(); err == nil {
			t.Errorf("%s should fail validation: %+v", name, p)
		}
	}
	custom := Pool{Name: "x", Runtime: "custom", Image: "img:1"}
	if err := custom.Validate(); err != nil {
		t.Errorf("custom with image: %v", err)
	}
	// Any flavor name is acceptable: the catalog only feeds the dropdown, so
	// removing a flavor never prevents the controller from starting.
	unknown := Pool{Name: "x", Runtime: "fedora-42", DockerSocket: true}
	if err := unknown.Validate(); err != nil || unknown.EffectiveImage() != RunnerImageRepo+":fedora-42" {
		t.Errorf("unknown flavor: %v %s", err, unknown.EffectiveImage())
	}
	long := Pool{Name: "x", Runtime: strings.Repeat("r", 63)}
	if err := long.Validate(); err != nil {
		t.Errorf("63-char runtime names are valid: %v", err)
	}
	long.Runtime = strings.Repeat("r", 64)
	if err := long.Validate(); err == nil {
		t.Error("64-char runtime names are not")
	}
}

func TestPool_GenerationIgnoresNameAndCountOnly(t *testing.T) {
	base := goodPool()
	same := base
	same.Name, same.Count = "other", 9
	if base.Generation() != same.Generation() {
		t.Error("name and count must not change the generation")
	}
	for _, mutate := range []func(*Pool){
		func(p *Pool) { p.Runtime = "debian" },
		func(p *Pool) { p.Image = "other:tag" },
		func(p *Pool) { p.Labels = "gpu" },
		func(p *Pool) { p.Group = "g" },
		func(p *Pool) { p.Ephemeral = true },
		func(p *Pool) { p.DockerSocket = false },
		func(p *Pool) { p.WorkBase = "/srv/x" },
		func(p *Pool) { p.ExtraEnv = "A=b" },
		func(p *Pool) { p.GracefulStopSeconds = 1 },
	} {
		p := base
		mutate(&p)
		if p.Generation() == base.Generation() {
			t.Errorf("generation unchanged after %+v", p)
		}
	}
	if len(base.Generation()) != 16 {
		t.Errorf("generation should be 16 hex chars, got %q", base.Generation())
	}
}

func TestPool_ListsAndNormalization(t *testing.T) {
	p := Pool{Name: " Debian ", Runtime: " Debian", Image: " img:1 ", Labels: " a, b ,,c", ExtraEnv: "A=1\n\n B=2 \n"}
	n := p.normalized()
	if n.Name != "Debian" || n.Runtime != "debian" || n.Image != "img:1" || n.Labels != "a,b,c" {
		t.Errorf("normalized: %+v", n)
	}
	if got := n.LabelList(); len(got) != 3 || got[2] != "c" {
		t.Errorf("LabelList: %v", got)
	}
	if got := n.ExtraEnvList(); len(got) != 2 || got[1] != "B=2" {
		t.Errorf("ExtraEnvList: %v", got)
	}
	if (Pool{}).LabelList() != nil {
		t.Error("empty labels must give a nil list")
	}
}
