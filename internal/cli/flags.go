package cli

import "github.com/spf13/cobra"

type fKind int

const (
	fString fKind = iota
	fBool
	fInt
	fStrings
)

type fl struct {
	kind  fKind
	name  string
	usage string
	str   string
	n      int
	on     bool
	hidden bool
}

func flagString(name, def, usage string) fl {
	return fl{kind: fString, name: name, str: def, usage: usage}
}

func flagBool(name, usage string) fl {
	return fl{kind: fBool, name: name, usage: usage}
}

func flagBoolDefault(name string, def bool, usage string) fl {
	return fl{kind: fBool, name: name, on: def, usage: usage}
}

// flagHiddenBool is a bool flag left out of help, for build and lint checks.
func flagHiddenBool(name, usage string) fl {
	return fl{kind: fBool, name: name, usage: usage, hidden: true}
}

func flagInt(name string, def int, usage string) fl {
	return fl{kind: fInt, name: name, n: def, usage: usage}
}

func flagStrings(name, usage string) fl {
	return fl{kind: fStrings, name: name, usage: usage}
}

func applyFlags(cmd *cobra.Command, flags []fl) {
	for _, flag := range flags {
		switch flag.kind {
		case fString:
			cmd.Flags().String(flag.name, flag.str, flag.usage)
		case fBool:
			cmd.Flags().Bool(flag.name, flag.on, flag.usage)
		case fInt:
			cmd.Flags().Int(flag.name, flag.n, flag.usage)
		case fStrings:
			cmd.Flags().StringSlice(flag.name, nil, flag.usage)
		default:
			panic("unknown flag kind for " + flag.name)
		}
		if flag.hidden {
			if err := cmd.Flags().MarkHidden(flag.name); err != nil {
				panic(err)
			}
		}
	}
}
