package provider

import (
	"errors"
	"fmt"
)

// ByHand builds the door a watcher runs with no provider file: an exec door
// running argv, one template per argument, or a stdout or exit door printing
// the signal line, which takes no argv. Its errors name no provider.
func ByHand(t Transport, argv []string) (*Door, error) {
	d := &Door{Name: "by-hand", Transport: t, Layer: 2}
	set := map[string]bool{}
	switch t {
	case TransportExec:
		d.Argv = argv
		set["argv"] = true
	case TransportStdout, TransportExit:
		if len(argv) > 0 {
			return nil, fmt.Errorf("the %s transport takes no command", t)
		}
	default:
		return nil, errors.New("the transport must be exec, stdout or exit")
	}
	err := d.check(set)
	if err == nil {
		d.applyDefaults("", set)
		err = d.compile(set)
	}
	if le, ok := errors.AsType[*LoadError](err); ok {
		return nil, fmt.Errorf("%s: %s", le.Key, le.Problem)
	}
	if err != nil {
		return nil, err
	}
	return d, nil
}
