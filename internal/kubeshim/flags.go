package kubeshim

import (
	"fmt"
	"strings"
)

// flagSpec describes one docker CLI option the way docker's own flag parser
// (spf13/pflag) sees it.
type flagSpec struct {
	// name is the long name ("env").
	name string
	// short is the one-letter shorthand ("e"), or "" for none.
	short string
	// value marks an option that takes a value. The others are booleans, which
	// may still carry one with "=" (--sig-proxy=false, -d=false).
	value bool
	// canonical names the option this one is a hidden alias of ("net" is
	// "network"), or "" when it is its own.
	canonical string
}

// flagSet is a table of options and the parser docker applies to them: GNU
// long options (--name value, --name=value), shorthands that may be bundled
// (-it) or carry their value attached (-eKEY=VALUE, -u=root), "--" to end the
// options, and parsing that stops at the first positional argument (the image
// of a run, the container of an exec: everything after it is the command).
type flagSet struct {
	long  map[string]flagSpec
	short map[byte]flagSpec
}

// newFlagSet indexes specs. A duplicate name or shorthand is a programming
// error in a table and panics.
func newFlagSet(specs []flagSpec) *flagSet {
	fs := &flagSet{long: make(map[string]flagSpec, len(specs)), short: make(map[byte]flagSpec)}
	for _, s := range specs {
		if _, dup := fs.long[s.name]; dup {
			panic("kubeshim: duplicate flag --" + s.name)
		}
		fs.long[s.name] = s
		if s.short != "" {
			if len(s.short) != 1 {
				panic("kubeshim: shorthand of --" + s.name + " is not one letter")
			}
			if _, dup := fs.short[s.short[0]]; dup {
				panic("kubeshim: duplicate shorthand -" + s.short)
			}
			fs.short[s.short[0]] = s
		}
	}
	return fs
}

// Flag is one option as parsed.
type Flag struct {
	// Name is the option's canonical long name, without dashes ("env",
	// "network" for --net). A hidden alias is reported under the name it
	// stands for.
	Name string `json:"name"`
	// Value is the option's value; "true" or "false" for a boolean.
	Value string `json:"value"`
}

// UnknownFlagError is an option the table does not know. Docker itself would
// refuse it too, unless it is newer than the table, which is why the shim
// refuses it rather than guess whether it takes a value.
type UnknownFlagError struct {
	// Flag is the option as written ("--foo", "-z").
	Flag string
}

func (e *UnknownFlagError) Error() string {
	return "unknown flag: " + e.Flag
}

// parse parses args up to the first positional argument and returns the
// options in order with the arguments that follow them (the first of which is
// the positional argument).
func (fs *flagSet) parse(args []string) ([]Flag, []string, error) {
	return fs.parseArgs(args, false)
}

// parseInterspersed parses options anywhere among the operands, as docker
// does for commands that take no command line of their own (inspect, ps), and
// returns the options and the operands. Everything after "--" is an operand.
func (fs *flagSet) parseInterspersed(args []string) ([]Flag, []string, error) {
	return fs.parseArgs(args, true)
}

func (fs *flagSet) parseArgs(args []string, interspersed bool) ([]Flag, []string, error) {
	var flags []Flag
	var operands []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			return flags, append(operands, args[i+1:]...), nil
		case arg == "-" || !strings.HasPrefix(arg, "-"):
			if !interspersed {
				return flags, args[i:], nil
			}
			operands = append(operands, arg)
		case strings.HasPrefix(arg, "--"):
			name, value, hasValue := strings.Cut(arg[2:], "=")
			spec, ok := fs.long[name]
			if !ok {
				return nil, nil, &UnknownFlagError{Flag: "--" + name}
			}
			switch {
			case !spec.value:
				v, err := boolValue(arg, value, hasValue)
				if err != nil {
					return nil, nil, err
				}
				flags = append(flags, Flag{Name: spec.canonicalName(), Value: v})
			case hasValue:
				flags = append(flags, Flag{Name: spec.canonicalName(), Value: value})
			case i+1 < len(args):
				i++
				flags = append(flags, Flag{Name: spec.canonicalName(), Value: args[i]})
			default:
				return nil, nil, fmt.Errorf("flag needs an argument: %s", arg)
			}
		default:
			parsed, consumedNext, err := fs.parseShorthands(arg, args[i+1:])
			if err != nil {
				return nil, nil, err
			}
			flags = append(flags, parsed...)
			if consumedNext {
				i++
			}
		}
	}
	return flags, operands, nil
}

// parseShorthands parses one "-abc" argument: bundled booleans, and at most
// one shorthand taking a value, whose value is the rest of the argument
// ("-eKEY=VALUE", "-u=root") or, when nothing follows it, the next argument.
func (fs *flagSet) parseShorthands(arg string, rest []string) ([]Flag, bool, error) {
	var flags []Flag
	letters := arg[1:]
	for len(letters) > 0 {
		c := letters[0]
		spec, ok := fs.short[c]
		if !ok {
			return nil, false, &UnknownFlagError{Flag: "-" + string(c)}
		}
		after := letters[1:]
		if !spec.value {
			if strings.HasPrefix(after, "=") {
				v, err := boolValue(arg, after[1:], true)
				if err != nil {
					return nil, false, err
				}
				return append(flags, Flag{Name: spec.canonicalName(), Value: v}), false, nil
			}
			flags = append(flags, Flag{Name: spec.canonicalName(), Value: "true"})
			letters = after
			continue
		}
		switch {
		case strings.HasPrefix(after, "=") && len(after) > 1:
			return append(flags, Flag{Name: spec.canonicalName(), Value: after[1:]}), false, nil
		case after != "":
			return append(flags, Flag{Name: spec.canonicalName(), Value: after}), false, nil
		case len(rest) > 0:
			return append(flags, Flag{Name: spec.canonicalName(), Value: rest[0]}), true, nil
		default:
			return nil, false, fmt.Errorf("flag needs an argument: '%c' in %s", c, arg)
		}
	}
	return flags, false, nil
}

// canonicalName is the name a parsed option is reported under.
func (s flagSpec) canonicalName() string {
	if s.canonical != "" {
		return s.canonical
	}
	return s.name
}

// boolValue normalises a boolean option's value as strconv.ParseBool (what
// pflag uses) reads it; a boolean given without a value is true.
func boolValue(arg, value string, hasValue bool) (string, error) {
	if !hasValue {
		return "true", nil
	}
	switch value {
	case "1", "t", "T", "TRUE", "true", "True":
		return "true", nil
	case "0", "f", "F", "FALSE", "false", "False":
		return "false", nil
	}
	return "", fmt.Errorf("invalid boolean value %q for %s", value, arg)
}

// lastValue returns the value of the last occurrence of name in flags.
func lastValue(flags []Flag, name string) (string, bool) {
	for i := len(flags) - 1; i >= 0; i-- {
		if flags[i].Name == name {
			return flags[i].Value, true
		}
	}
	return "", false
}

// values returns the value of every occurrence of name in flags, in order.
func values(flags []Flag, name string) []string {
	var out []string
	for _, f := range flags {
		if f.Name == name {
			out = append(out, f.Value)
		}
	}
	return out
}
