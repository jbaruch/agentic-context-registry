package cli

import (
	"fmt"
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
		usage:                    "acr migrate tessl [--non-interactive] [--dry-run]\n  acr migrate tessl-plugin [PATH] [--dry-run] [--repository URL] [--accept-agent-widening]",
		summary:                  "Migrate a Tessl consumer project or plugin package",
		minimumArguments:         1,
		maximumArguments:         2,
		allowDryRun:              true,
		allowNonInteractive:      true,
		allowRepository:          true,
		allowAcceptAgentWidening: true,
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
			invocation.Subcommand = "tessl-plugin"
			invocation.PublicationPath = "."
			if len(positionals) == 2 {
				invocation.PublicationPath = positionals[1]
			}
			invocation.Repository = flags.repository
			invocation.AcceptAgentWidening = flags.acceptAgentWidening
		default:
			return Invocation{}, false, usageError("unsupported migration target %q; usage: %s", positionals[0], spec.usage)
		}
	case CommandFreshness:
		if positionals[0] != "run" {
			return Invocation{}, false, usageError("unsupported freshness subcommand %q; usage: %s", positionals[0], spec.usage)
		}
		invocation.Subcommand = positionals[0]
	}

	return invocation, false, nil
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
	builder.WriteString("  -h, --help          Show command help\n")
	return builder.String()
}
