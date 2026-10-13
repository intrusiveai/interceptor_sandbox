package catalogcli

import (
	"errors"
	"flag"
	"os"

	"github.com/intrusiveai-engineering/releases/internal/catalog"
)

func Run(arguments []string) error {
	if len(arguments) < 2 {
		return errors.New("expected a command and subcommand")
	}
	switch arguments[0] + " " + arguments[1] {
	case "catalog merge":
		set := flag.NewFlagSet("catalog merge", flag.ContinueOnError)
		product := set.String("product", "attack_harness", "catalog product")
		current := set.String("current", "", "existing catalog; omit for first publication")
		entry := set.String("entry", "", "one product release entry")
		output := set.String("output", "", "merged catalog output")
		if err := set.Parse(arguments[2:]); err != nil {
			return err
		}
		if *entry == "" || *output == "" || set.NArg() != 0 {
			return errors.New("catalog merge requires --entry and --output")
		}
		entryJSON, err := os.ReadFile(*entry)
		if err != nil {
			return err
		}
		var currentJSON []byte
		if *current != "" {
			currentJSON, err = os.ReadFile(*current)
			if err != nil {
				return err
			}
		}
		raw, err := catalog.MergeCatalogEntry(*product, currentJSON, entryJSON)
		if err != nil {
			return err
		}
		return os.WriteFile(*output, raw, 0o644)
	default:
		return errors.New("unsupported publication command")
	}
}
