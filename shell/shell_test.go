package shell

import (
	"runtime"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	sh := Resolve()
	if sh.Path == "" {
		t.Fatal("expected non-empty shell path")
	}
	if len(sh.Args) == 0 {
		t.Fatal("expected shell args")
	}
	switch runtime.GOOS {
	case "windows":
		if sh.IsBash {
			if isWSLStub(sh.Path) {
				t.Fatalf("resolved to WSL stub: %s", sh.Path)
			}
			if !strings.Contains(strings.ToLower(sh.Path), "bash") {
				t.Fatalf("IsBash but path is not bash: %s", sh.Path)
			}
		} else if sh.Name() != "cmd" {
			t.Fatalf("expected cmd fallback, got %q (%s)", sh.Name(), sh.Path)
		}
	default:
		if sh.Name() != "bash" && sh.Name() != "sh" {
			t.Fatalf("expected bash or sh on unix, got %q", sh.Name())
		}
	}
}

func TestResolveCached(t *testing.T) {
	a, b := Resolve(), Resolve()
	if a.Path != b.Path || a.IsBash != b.IsBash || len(a.Args) != len(b.Args) {
		t.Fatalf("expected cached result, got %+v vs %+v", a, b)
	}
}

func TestName(t *testing.T) {
	cases := []struct {
		sh   Shell
		want string
	}{
		{Shell{Path: `C:\Program Files\Git\bin\bash.exe`, IsBash: true}, "bash"},
		{Shell{Path: "cmd"}, "cmd"},
		{Shell{Path: "sh"}, "sh"},
	}
	for _, c := range cases {
		if got := c.sh.Name(); got != c.want {
			t.Errorf("Name() = %q, want %q", got, c.want)
		}
	}
}
