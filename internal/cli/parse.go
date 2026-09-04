package cli

import (
	"fmt"
	"sort"
	"strings"
)

type commandSpec struct {
	command                  Command
	usage                    string
	summary                  string
	minimumArguments         int
	maximumArguments         int
	allowDryRun              bool
	allowNonInteractive      bool
	allowAgents              bool
	allowFreshness           bool
	allowPolicy              bool
	allowDowngrade           bool
	allowRepository          bool
	allowAcceptAgentWidening bool
	allowMigration           bool
	// subcommands lists the exact leaves this command accepts as its first
	// positional argument. An empty list means the command is its own leaf.
	subcommands []string
	// subcommandNoun names a subcommand in this command's refusal.
	subcommandNoun string
}

var commandOrder = []Command{
	CommandInit,
	CommandInstall,
	CommandRealize,
	CommandList,
	CommandOutdated,
	CommandFreshness,
	CommandUpdate,
	CommandResume,
	CommandUninstall,
	CommandCheck,
	CommandPublish,
	CommandMigrate,
}

var commandSpecs = map[Command]commandSpec{
	CommandInit: {
		command:             CommandInit,
		usage:               "acr init [--agent NAME] [--freshness POLICY] [--non-interactive] [--dry-run]",
		summary:             "Initialize project agent configuration",
		allowDryRun:         true,
		allowNonInteractive: true,
		allowAgents:         true,
		allowFreshness:      true,
	},
	CommandInstall: {
		command:             CommandInstall,
		usage:               "acr install [SOURCE[@VERSION]] [--hold | --pin] [--agent NAME] [--freshness POLICY] [--non-interactive] [--dry-run]",
		summary:             "Install a package or reconcile declared dependencies",
		maximumArguments:    1,
		allowDryRun:         true,
		allowNonInteractive: true,
		allowAgents:         true,
		allowFreshness:      true,
		allowDowngrade:      true,
	},
	CommandRealize: {
		command:     CommandRealize,
		usage:       "acr realize [--agent NAME] [--dry-run]",
		summary:     "Realize locked packages into native agent layouts",
		allowDryRun: true,
		allowAgents: true,
	},
	CommandList: {
		command: CommandList,
		usage:   "acr list",
		summary: "List declared and resolved dependencies",
	},
	CommandOutdated: {
		command: CommandOutdated,
		usage:   "acr outdated",
		summary: "Check latest dependencies for newer stable releases",
	},
	CommandFreshness: {
		command:          CommandFreshness,
		usage:            "acr freshness run [--policy POLICY]",
		summary:          "Run the throttled session-start freshness policy",
		minimumArguments: 1,
		maximumArguments: 1,
		allowPolicy:      true,
		subcommands:      []string{"run"},
		subcommandNoun:   "freshness subcommand",
	},
	CommandUpdate: {
		command:          CommandUpdate,
		usage:            "acr update [SOURCE] [--dry-run]",
		summary:          "Update one dependency or all eligible dependencies",
		maximumArguments: 1,
		allowDryRun:      true,
	},
	CommandResume: {
		command:          CommandResume,
		usage:            "acr resume SOURCE [--dry-run]",
		summary:          "Clear a rollback hold and resume latest",
		minimumArguments: 1,
		maximumArguments: 1,
		allowDryRun:      true,
	},
	CommandUninstall: {
		command:          CommandUninstall,
		usage:            "acr uninstall SOURCE [--dry-run]",
		summary:          "Remove a dependency and its owned artifacts",
		minimumArguments: 1,
		maximumArguments: 1,
		allowDryRun:      true,
	},
	CommandCheck: {
		command:     CommandCheck,
		usage:       "acr check [--agent NAME]",
		summary:     "Check project state without applying changes",
		allowAgents: true,
	},
	CommandPublish: {
		command:          CommandPublish,
		usage:            "acr publish [PATH] [--dry-run]",
		summary:          "Validate and publish an immutable package release",
		maximumArguments: 1,
		allowDryRun:      true,
	},
	CommandMigrate: {
		command:                  CommandMigrate,
		usage:                    "acr migrate tessl [--mapping-file PATH] [--map FROM=SOURCE[@REQUESTED]] [--vendor-unmapped] [--finalize] [--non-interactive] [--dry-run]\n  acr migrate tessl-plugin [PATH] [--dry-run] [--repository URL] [--accept-agent-widening]",
		summary:                  "Migrate a Tessl consumer project or plugin package",
		minimumArguments:         1,
		maximumArguments:         2,
		allowDryRun:              true,
		allowNonInteractive:      true,
		allowRepository:          true,
		allowAcceptAgentWidening: true,
		allowMigration:           true,
		subcommands:              []string{"tessl", "tessl-plugin"},
		subcommandNoun:           "migration target",
	},
}

type parsedFlags struct {
	output              OutputFormat
	projectDirectory    string
	dryRun              bool
	nonInteractive      bool
	agents              []string
	freshness           FreshnessPolicy
	freshnessExplicit   bool
	downgrade           DowngradeChoice
	help                bool
	repository          string
	acceptAgentWidening bool
	mappingFile         string
	mappings            []string
	finalize            bool
	vendorUnmapped      bool
}

func parseInvocation(command Command, args []string) (Invocation, bool, error) {
	spec := commandSpecs[command]
	flags, positionals, err := parseFlags(spec, args)
	if err != nil {
		return Invocation{}, false, err
	}
	if flags.help {
		return Invocation{}, true, nil
	}
	if len(positionals) < spec.minimumArguments || len(positionals) > spec.maximumArguments {
		return Invocation{}, false, usageError("usage: %s", spec.usage)
	}

	invocation := Invocation{
		Command:           command,
		ProjectDirectory:  flags.projectDirectory,
		Output:            flags.output,
		DryRun:            flags.dryRun,
		NonInteractive:    flags.nonInteractive,
		Agents:            flags.agents,
		Freshness:         flags.freshness,
		FreshnessExplicit: flags.freshnessExplicit,
		MappingFile:       flags.mappingFile,
		Mappings:          flags.mappings,
		Finalize:          flags.finalize,
		VendorUnmapped:    flags.vendorUnmapped,
	}

	if len(spec.subcommands) != 0 && !acceptsSubcommand(spec, positionals[0]) {
		return Invocation{}, false, usageError("unsupported %s %q; usage: %s", spec.subcommandNoun, positionals[0], spec.usage)
	}

	switch command {
	case CommandInstall:
		if len(positionals) == 0 {
			if flags.downgrade != DowngradeUnset {
				return Invocation{}, false, usageError("--%s requires an explicit SOURCE@VERSION; usage: %s", flags.downgrade, spec.usage)
			}
			invocation.Reconcile = true
			break
		}
		invocation.Source, invocation.RequestedVersion, err = parseInstallSource(positionals[0])
		if err != nil {
			return Invocation{}, false, err
		}
		if flags.downgrade != DowngradeUnset && invocation.RequestedVersion == "latest" {
			return Invocation{}, false, usageError("--%s requires an explicit version; SOURCE without @VERSION requests latest", flags.downgrade)
		}
		invocation.Downgrade = flags.downgrade
	case CommandUpdate, CommandResume, CommandUninstall:
		if len(positionals) != 0 {
			invocation.Source = positionals[0]
		}
	case CommandPublish:
		invocation.PublicationPath = "."
		if len(positionals) != 0 {
			invocation.PublicationPath = positionals[0]
		}
	case CommandMigrate:
		switch positionals[0] {
		case "tessl":
			if len(positionals) != 1 {
				return Invocation{}, false, usageError("usage: acr migrate tessl [--non-interactive] [--dry-run]")
			}
			if flags.repository != "" || flags.acceptAgentWidening {
				return Invocation{}, false, usageError("--repository and --accept-agent-widening are only supported by acr migrate tessl-plugin")
			}
			invocation.Subcommand = "tessl"
		case "tessl-plugin":
			if flags.nonInteractive {
				return Invocation{}, false, usageError("--non-interactive is not supported by acr migrate tessl-plugin; remove the flag")
			}
			if flags.mappingFile != "" || len(flags.mappings) != 0 || flags.finalize || flags.vendorUnmapped {
				return Invocation{}, false, usageError("--mapping-file, --map, --vendor-unmapped, and --finalize are only supported by acr migrate tessl")
			}
			invocation.Subcommand = "tessl-plugin"
			invocation.PublicationPath = "."
			if len(positionals) == 2 {
				invocation.PublicationPath = positionals[1]
			}
			invocation.Repository = flags.repository
			invocation.AcceptAgentWidening = flags.acceptAgentWidening
		}
	case CommandFreshness:
		invocation.Subcommand = positionals[0]
	}

	return invocation, false, nil
}

func acceptsSubcommand(spec commandSpec, value string) bool {
	for _, subcommand := range spec.subcommands {
		if subcommand == value {
			return true
		}
	}
	return false
}

// Leaf is one executable acr invocation: a command, and the subcommand it
// requires when the command has one.
type Leaf struct {
	Command    string
	Subcommand string
}

// String returns the leaf as a user types it, without options.
func (leaf Leaf) String() string {
	if leaf.Subcommand == "" {
		return leaf.Command
	}
	return leaf.Command + " " + leaf.Subcommand
}

// Args returns the argument prefix that selects this leaf.
func (leaf Leaf) Args() []string {
	if leaf.Subcommand == "" {
		return []string{leaf.Command}
	}
	return []string{leaf.Command, leaf.Subcommand}
}

// CommandSurface is the parser's dispatch registry expressed as data: every
// command commandFor accepts, the subcommands each one requires, the display
// order, and the meta commands the runner answers before the registry.
//
// Surface returns the shipped one and LeavesOf expands any surface, so a test
// can register a command the way a future change would and watch the inventory
// that results. Enumerating the registry rather than the display order is what
// keeps a command from being dispatchable and invisible at the same time.
type CommandSurface struct {
	// Subcommands maps every dispatchable command to the subcommands it
	// accepts. An empty list means the command is its own leaf.
	Subcommands map[string][]string
	// Order lists commands in display order. A dispatchable command missing
	// from Order is still a leaf, sorted after the ordered ones.
	Order []string
	// Meta lists the commands the runner answers before the registry.
	Meta []string
}

// Surface returns the shipped dispatch registry as data. The maps and slices
// are copies, so a caller can mutate the result without touching the parser.
func Surface() CommandSurface {
	surface := CommandSurface{
		Subcommands: make(map[string][]string, len(commandSpecs)),
		Order:       make([]string, 0, len(commandSpecs)),
		Meta:        make([]string, 0, len(metaCommandOrder)),
	}
	for command, spec := range commandSpecs {
		surface.Subcommands[string(command)] = append([]string(nil), spec.subcommands...)
	}
	for _, command := range dispatchableCommands() {
		surface.Order = append(surface.Order, string(command))
	}
	for _, meta := range metaCommandOrder {
		surface.Meta = append(surface.Meta, meta.name)
	}
	return surface
}

// LeavesOf expands one surface into its executable leaves: every dispatchable
// command, expanded by the subcommands it accepts, followed by the meta
// commands. A command the surface dispatches but never orders still becomes a
// leaf, which is the case that used to escape the inventory entirely.
func LeavesOf(surface CommandSurface) []Leaf {
	leaves := make([]Leaf, 0, len(surface.Subcommands)+len(surface.Meta))
	ordered := make(map[string]bool, len(surface.Order))
	for _, command := range surface.Order {
		subcommands, dispatchable := surface.Subcommands[command]
		if !dispatchable || ordered[command] {
			continue
		}
		ordered[command] = true
		leaves = append(leaves, expandLeaf(command, subcommands)...)
	}
	unordered := make([]string, 0, len(surface.Subcommands))
	for command := range surface.Subcommands {
		if !ordered[command] {
			unordered = append(unordered, command)
		}
	}
	sort.Strings(unordered)
	for _, command := range unordered {
		leaves = append(leaves, expandLeaf(command, surface.Subcommands[command])...)
	}
	for _, meta := range surface.Meta {
		leaves = append(leaves, Leaf{Command: meta})
	}
	return leaves
}

// Leaves returns every executable leaf of the shipped command surface. It is
// the same data the parser dispatches on and help renders, so a command that
// cannot be reached from this list cannot be reached from a shell either.
func Leaves() []Leaf {
	return LeavesOf(Surface())
}

func expandLeaf(command string, subcommands []string) []Leaf {
	if len(subcommands) == 0 {
		return []Leaf{{Command: command}}
	}
	leaves := make([]Leaf, 0, len(subcommands))
	for _, subcommand := range subcommands {
		leaves = append(leaves, Leaf{Command: command, Subcommand: subcommand})
	}
	return leaves
}

// dispatchableCommands returns every command commandFor accepts, in display
// order: the curated order first, then any command registered without one, so
// registering a command is enough to make it visible to help and the inventory.
func dispatchableCommands() []Command {
	commands := make([]Command, 0, len(commandSpecs))
	ordered := make(map[Command]bool, len(commandOrder))
	for _, command := range commandOrder {
		if _, dispatchable := commandSpecs[command]; !dispatchable || ordered[command] {
			continue
		}
		ordered[command] = true
		commands = append(commands, command)
	}
	unordered := make([]string, 0, len(commandSpecs))
	for command := range commandSpecs {
		if !ordered[command] {
			unordered = append(unordered, string(command))
		}
	}
	sort.Strings(unordered)
	for _, command := range unordered {
		commands = append(commands, Command(command))
	}
	return commands
}

func parseFlags(spec commandSpec, args []string) (parsedFlags, []string, error) {
	flags := parsedFlags{
		output:           OutputText,
		projectDirectory: ".",
	}
	positionals := make([]string, 0, len(args))

	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			positionals = append(positionals, args[index+1:]...)
			break
		}
		if !strings.HasPrefix(argument, "-") || argument == "-" {
			positionals = append(positionals, argument)
			continue
		}

		name, inlineValue, hasInlineValue := strings.Cut(argument, "=")
		switch name {
		case "--help", "-h":
			if hasInlineValue {
				return parsedFlags{}, nil, usageError("%s does not accept a value; remove the value", name)
			}
			flags.help = true
		case "--json":
			if hasInlineValue {
				return parsedFlags{}, nil, usageError("--json does not accept a value; remove the value")
			}
			flags.output = OutputJSON
		case "--project":
			value, next, err := flagValue(args, index, inlineValue, hasInlineValue, name)
			if err != nil {
				return parsedFlags{}, nil, err
			}
			index = next
			flags.projectDirectory = value
		case "--dry-run":
			if !spec.allowDryRun {
				return parsedFlags{}, nil, unsupportedFlagError(spec, name)
			}
			if hasInlineValue {
				return parsedFlags{}, nil, usageError("--dry-run does not accept a value; remove the value")
			}
			flags.dryRun = true
		case "--non-interactive":
			if !spec.allowNonInteractive {
				return parsedFlags{}, nil, unsupportedFlagError(spec, name)
			}
			if hasInlineValue {
				return parsedFlags{}, nil, usageError("--non-interactive does not accept a value; remove the value")
			}
			flags.nonInteractive = true
		case "--mapping-file":
			if !spec.allowMigration {
				return parsedFlags{}, nil, unsupportedFlagError(spec, name)
			}
			value, next, err := flagValue(args, index, inlineValue, hasInlineValue, name)
			if err != nil {
				return parsedFlags{}, nil, err
			}
			index = next
			if flags.mappingFile != "" && flags.mappingFile != value {
				return parsedFlags{}, nil, usageError("--mapping-file may be specified only once")
			}
			flags.mappingFile = value
		case "--map":
			if !spec.allowMigration {
				return parsedFlags{}, nil, unsupportedFlagError(spec, name)
			}
			value, next, err := flagValue(args, index, inlineValue, hasInlineValue, name)
			if err != nil {
				return parsedFlags{}, nil, err
			}
			index = next
			from, _, found := strings.Cut(value, "=")
			if !found || from == "" {
				return parsedFlags{}, nil, usageError("invalid --map %q; use FROM=github:owner/repository[@REQUESTED]", value)
			}
			for _, prior := range flags.mappings {
				priorFrom, _, _ := strings.Cut(prior, "=")
				if priorFrom == from && prior != value {
					return parsedFlags{}, nil, usageError("--map for %s is specified more than once with different values", from)
				}
			}
			flags.mappings = append(flags.mappings, value)
		case "--finalize":
			if !spec.allowMigration {
				return parsedFlags{}, nil, unsupportedFlagError(spec, name)
			}
			if hasInlineValue {
				return parsedFlags{}, nil, usageError("--finalize does not accept a value; remove the value")
			}
			flags.finalize = true
		case "--vendor-unmapped":
			if !spec.allowMigration {
				return parsedFlags{}, nil, unsupportedFlagError(spec, name)
			}
			if hasInlineValue {
				return parsedFlags{}, nil, usageError("--vendor-unmapped does not accept a value; remove the value")
			}
			flags.vendorUnmapped = true
		case "--hold", "--pin":
			if !spec.allowDowngrade {
				return parsedFlags{}, nil, unsupportedFlagError(spec, name)
			}
			if hasInlineValue {
				return parsedFlags{}, nil, usageError("%s does not accept a value; remove the value", name)
			}
			choice := DowngradeChoice(strings.TrimPrefix(name, "--"))
			if flags.downgrade != DowngradeUnset && flags.downgrade != choice {
				return parsedFlags{}, nil, usageError("--hold and --pin are mutually exclusive; choose a temporary rollback or a permanent pin")
			}
			flags.downgrade = choice
		case "--agent":
			if !spec.allowAgents {
				return parsedFlags{}, nil, unsupportedFlagError(spec, name)
			}
			value, next, err := flagValue(args, index, inlineValue, hasInlineValue, name)
			if err != nil {
				return parsedFlags{}, nil, err
			}
			index = next
			flags.agents = append(flags.agents, value)
		case "--freshness":
			if !spec.allowFreshness {
				return parsedFlags{}, nil, unsupportedFlagError(spec, name)
			}
			value, next, err := flagValue(args, index, inlineValue, hasInlineValue, name)
			if err != nil {
				return parsedFlags{}, nil, err
			}
			index = next
			flags.freshness, err = parseFreshness(value)
			if err != nil {
				return parsedFlags{}, nil, err
			}
			flags.freshnessExplicit = true
		case "--policy":
			if !spec.allowPolicy {
				return parsedFlags{}, nil, unsupportedFlagError(spec, name)
			}
			value, next, err := flagValue(args, index, inlineValue, hasInlineValue, name)
			if err != nil {
				return parsedFlags{}, nil, err
			}
			index = next
			flags.freshness, err = parseFreshness(value)
			if err != nil {
				return parsedFlags{}, nil, err
			}
			flags.freshnessExplicit = true
		case "--repository":
			if !spec.allowRepository {
				return parsedFlags{}, nil, unsupportedFlagError(spec, name)
			}
			value, next, err := flagValue(args, index, inlineValue, hasInlineValue, name)
			if err != nil {
				return parsedFlags{}, nil, err
			}
			index = next
			flags.repository = value
		case "--accept-agent-widening":
			if !spec.allowAcceptAgentWidening {
				return parsedFlags{}, nil, unsupportedFlagError(spec, name)
			}
			if hasInlineValue {
				return parsedFlags{}, nil, usageError("--accept-agent-widening does not accept a value; remove the value")
			}
			flags.acceptAgentWidening = true
		default:
			return parsedFlags{}, nil, usageError("unknown flag %q for %s; run 'acr %s --help' for supported options", name, spec.command, spec.command)
		}
	}

	if (spec.allowFreshness || spec.allowPolicy) && flags.freshness == "" {
		flags.freshness = FreshnessOutdated
	}
	return flags, positionals, nil
}

func unsupportedFlagError(spec commandSpec, name string) error {
	return usageError("%s is not supported by %s; run 'acr %s --help' for supported options", name, spec.command, spec.command)
}

func flagValue(args []string, index int, inlineValue string, hasInlineValue bool, name string) (string, int, error) {
	if hasInlineValue {
		if inlineValue == "" {
			return "", index, usageError("%s requires a non-empty value", name)
		}
		return inlineValue, index, nil
	}
	if index+1 >= len(args) || strings.HasPrefix(args[index+1], "-") {
		return "", index, usageError("%s requires a value", name)
	}
	if args[index+1] == "" {
		return "", index, usageError("%s requires a non-empty value", name)
	}
	return args[index+1], index + 1, nil
}

func parseFreshness(value string) (FreshnessPolicy, error) {
	policy := FreshnessPolicy(value)
	switch policy {
	case FreshnessOutdated, FreshnessInstall, FreshnessNone:
		return policy, nil
	default:
		return "", usageError("invalid freshness policy %q; use outdated, install, or none", value)
	}
}

func parseInstallSource(value string) (string, string, error) {
	if value == "" {
		return "", "", usageError("install source must not be empty; provide SOURCE or omit SOURCE to reconcile declared dependencies")
	}
	if strings.Count(value, "@") > 1 {
		return "", "", usageError("install source must use SOURCE@VERSION and VERSION must not contain @")
	}
	separator := strings.LastIndex(value, "@")
	if separator < 0 {
		return value, "latest", nil
	}
	if separator == 0 || separator == len(value)-1 {
		return "", "", usageError("install source must use SOURCE@VERSION with non-empty values")
	}
	return value[:separator], value[separator+1:], nil
}

func commandFor(value string) (Command, bool) {
	command := Command(value)
	_, ok := commandSpecs[command]
	return command, ok
}

func helpFor(command Command) string {
	spec := commandSpecs[command]
	var builder strings.Builder
	fmt.Fprintf(&builder, "Usage:\n  %s\n\n%s.\n\nOptions:\n", spec.usage, spec.summary)
	builder.WriteString("  --json              Emit machine-readable JSON\n")
	builder.WriteString("  --project PATH      Use PATH as the project directory (default .)\n")
	if spec.allowDryRun {
		builder.WriteString("  --dry-run           Plan without applying changes\n")
	}
	if spec.allowNonInteractive {
		builder.WriteString("  --non-interactive   Never prompt for input\n")
	}
	if spec.allowAgents {
		builder.WriteString("  --agent NAME        Select an agent; repeat for multiple agents\n")
	}
	if spec.allowFreshness {
		builder.WriteString("  --freshness POLICY  Use outdated, install, or none (default outdated)\n")
	}
	if spec.allowDowngrade {
		builder.WriteString("  --hold              Roll back a latest dependency temporarily behind a resume barrier\n")
		builder.WriteString("  --pin               Replace latest with a permanent pin\n")
	}
	if spec.allowPolicy {
		builder.WriteString("  --policy POLICY     Override agents.yaml with outdated, install, or none\n")
	}
	if spec.allowRepository {
		builder.WriteString("  --repository URL    Set source.repository when the Tessl manifest omits it\n")
	}
	if spec.allowAcceptAgentWidening {
		builder.WriteString("  --accept-agent-widening  Convert nativeHooks that would fire on additional agents\n")
	}
	if spec.allowMigration {
		builder.WriteString("  --mapping-file PATH Load Tessl-to-GitHub mappings from YAML\n")
		builder.WriteString("  --map FROM=SOURCE   Map one Tessl package; repeat for multiple packages\n")
		builder.WriteString("  --vendor-unmapped   Copy unmapped packages into .agents/vendor\n")
		builder.WriteString("  --finalize          Remove Tessl-owned output after safety checks\n")
	}
	builder.WriteString("  -h, --help          Show command help\n")
	return builder.String()
}
