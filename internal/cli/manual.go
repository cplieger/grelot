package cli

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
)

// manual is grelot help: how to work with grelot, then every command.
//
//go:embed manual.md
var manual string

// section is the manual's section for verb, from its "### grelot <verb>"
// heading to the next heading; "" when it has none.
func section(verb string) string {
	head := "### grelot " + verb + "\n"
	i := strings.Index(manual, head)
	if i < 0 {
		return ""
	}
	body := manual[i:]
	if j := strings.Index(body[len(head):], "\n#"); j >= 0 {
		body = body[:len(head)+j+1]
	}
	return body
}

func (r *run) help(_ context.Context, args []string) error {
	switch len(args) {
	case 0:
		fmt.Fprint(r.Stdout, manual)
		return nil
	case 1:
		if s := section(args[0]); s != "" {
			fmt.Fprint(r.Stdout, s)
			return nil
		}
	}
	return usagef(r.verb, "no command %q: grelot help lists every command", strings.Join(args, " "))
}
