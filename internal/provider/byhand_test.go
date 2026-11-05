package provider

import (
	"slices"
	"testing"
)

const handLine = "[grelot:3] grelot (automated, not the user): 1 new; run grelot inbox"

func TestByHandExec(t *testing.T) {
	d, err := ByHand(TransportExec, []string{"notify-send", "--", "{{.Line}}"})
	if err != nil {
		t.Fatalf("ByHand: %v", err)
	}
	argv, err := d.RenderArgv(&Data{Line: handLine})
	if err != nil {
		t.Fatalf("RenderArgv: %v", err)
	}
	if want := []string{"notify-send", "--", handLine}; !slices.Equal(argv, want) {
		t.Errorf("argv = %q, want %q", argv, want)
	}
	if d.Layer != 2 {
		t.Errorf("layer = %d, want 2", d.Layer)
	}
}

func TestByHandLine(t *testing.T) {
	for _, tr := range []Transport{TransportStdout, TransportExit} {
		t.Run(string(tr), func(t *testing.T) {
			d, err := ByHand(tr, nil)
			if err != nil {
				t.Fatalf("ByHand: %v", err)
			}
			if got, err := d.RenderLine(&Data{Line: handLine}); err != nil || got != handLine {
				t.Errorf("RenderLine = %q, %v; want the signal line", got, err)
			}
			if got, err := d.RenderEnd(&Data{Line: "end"}); err != nil || got != "end" {
				t.Errorf("RenderEnd = %q, %v; want the end line", got, err)
			}
			if d.EndExit != 1 {
				t.Errorf("end_exit = %d, want 1", d.EndExit)
			}
		})
	}
}

func TestByHandRefuses(t *testing.T) {
	cases := []struct {
		name string
		tr   Transport
		argv []string
	}{
		{"http", TransportHTTP, nil},
		{"unknown transport", "socket", nil},
		{"exec with no argv", TransportExec, nil},
		{"exec with an empty program", TransportExec, []string{"", "x"}},
		{"exec with a bad template", TransportExec, []string{"notify", "{{.Nope}}"}},
		{"stdout with argv", TransportStdout, []string{"echo"}},
		{"exit with argv", TransportExit, []string{"echo"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if d, err := ByHand(c.tr, c.argv); err == nil {
				t.Errorf("ByHand = %+v, want a refusal", d)
			}
		})
	}
}
