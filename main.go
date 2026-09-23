// Command secrets-entrypoint loads secrets from one or more files into
// the process environment, reports what it loaded, and then replaces
// itself (via execve) with the real application -- so the real app
// inherits PID 1 and receives signals directly, with no wrapper process
// left running.
//
// Usage:
//
//	secrets-entrypoint [flags] -- <command> [args...]
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"syscall"
)

const defaultConfigPath = "/vault/secrets/config"

type settings struct {
	configEntries []string
	format        string
	quiet         bool
}

// stringList implements flag.Value to allow -config to repeat.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	cfg, cmdArgs, err := parseFlags(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "secrets-entrypoint:", err)
		os.Exit(2)
	}

	// run only returns on failure -- on success it has already execve'd
	// into the target command.
	err = run(cfg, cmdArgs)
	fmt.Fprintln(os.Stderr, "secrets-entrypoint:", err)
	os.Exit(1)
}

func parseFlags(args []string) (*settings, []string, error) {
	fs := flag.NewFlagSet("secrets-entrypoint", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: secrets-entrypoint [flags] -- <command> [args...]")
		fs.PrintDefaults()
	}

	var configFlags stringList
	fs.Var(&configFlags, "config", "literal path or glob pattern for a secrets file (repeatable)")
	formatFlag := fs.String("format", "auto", "format for every -config entry: auto|shell|json")
	quietFlag := fs.Bool("quiet", false, "suppress the stdout \"Loaded Secret Keys\" banner")

	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}

	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	configEntries := []string(configFlags)
	if len(configEntries) == 0 {
		if v, ok := os.LookupEnv("SECRETS_CONFIG_PATH"); ok {
			configEntries = splitConfigPath(v)
		}
		if len(configEntries) == 0 {
			configEntries = []string{defaultConfigPath}
		}
	}

	format := *formatFlag
	if !explicit["format"] {
		if v, ok := os.LookupEnv("SECRETS_CONFIG_FORMAT"); ok && v != "" {
			format = v
		}
	}
	switch format {
	case "auto", "shell", "json":
	default:
		return nil, nil, fmt.Errorf("invalid -format %q: must be auto, shell, or json", format)
	}

	quiet := *quietFlag
	if !explicit["quiet"] {
		if v, ok := os.LookupEnv("SECRETS_QUIET"); ok {
			quiet = parseBoolish(v)
		}
	}

	return &settings{configEntries: configEntries, format: format, quiet: quiet}, fs.Args(), nil
}

func splitConfigPath(v string) []string {
	parts := strings.Split(v, ":")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseBoolish(v string) bool {
	return v == "1" || strings.EqualFold(v, "true")
}

func run(cfg *settings, cmdArgs []string) error {
	if len(cmdArgs) == 0 {
		return fmt.Errorf("no command given after --; usage: secrets-entrypoint [flags] -- <command> [args...]")
	}

	result, err := loadSecrets(cfg.configEntries, cfg.format)
	if err != nil {
		return err
	}

	if !cfg.quiet {
		printBanner(result.Values)
	}

	ReportOutcome(result)

	env := buildEnviron(result.Values)

	target, err := exec.LookPath(cmdArgs[0])
	if err != nil {
		return fmt.Errorf("resolving command %q: %w", cmdArgs[0], err)
	}

	// argv[0] stays exactly as the caller gave it; only the resolved
	// path used to load the binary changes.
	err = syscall.Exec(target, cmdArgs, env)
	// syscall.Exec only returns on failure -- on success this process
	// image is already gone.
	return fmt.Errorf("exec %q: %w", target, err)
}

func printBanner(values map[string]string) {
	fmt.Println("=== Loaded Secret Keys ===")
	for _, k := range sortedKeys(values) {
		fmt.Println(k)
	}
	fmt.Println("==========================")
}

// buildEnviron merges loaded secrets on top of the inherited environment,
// secrets winning on name collision, with exactly one entry per key in
// the result.
func buildEnviron(secrets map[string]string) []string {
	merged := make(map[string]string, len(secrets))
	for _, kv := range os.Environ() {
		if idx := strings.IndexByte(kv, '='); idx >= 0 {
			merged[kv[:idx]] = kv[idx+1:]
		}
	}
	for k, v := range secrets {
		merged[k] = v
	}

	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]string, 0, len(merged))
	for _, k := range keys {
		out = append(out, k+"="+merged[k])
	}
	return out
}
