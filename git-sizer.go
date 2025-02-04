package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/pprof"
	"strconv"

	"github.com/github/git-sizer/git"
	"github.com/github/git-sizer/isatty"
	"github.com/github/git-sizer/sizes"

	"github.com/spf13/pflag"
)

const Usage = `usage: git-sizer [OPTS]

  -v, --verbose                report all statistics, whether concerning or not
      --threshold THRESHOLD    minimum level of concern (i.e., number of stars)
                               that should be reported. Default:
                               '--threshold=1'.
      --critical               only report critical statistics
      --names=[none|hash|full] display names of large objects in the specified
                               style: 'none' (omit footnotes entirely), 'hash'
                               (show only the SHA-1s of objects), or 'full'
                               (show full names). Default is '--names=full'.
  -j, --json                   output results in JSON format
      --json-version=[1|2]     choose which JSON format version to output.
                               Default: --json-version=1.
      --[no-]progress          report (don't report) progress to stderr.
      --version                only report the git-sizer version number

 Reference selection:

 By default, git-sizer processes all Git objects that are reachable from any
 reference. The following options can be used to limit which references to
 include. The last rule matching a reference determines whether that reference
 is processed:

      --branches               process branches
      --tags                   process tags
      --remotes                process remote refs
      --include PREFIX         process references with the specified PREFIX
                               (e.g., '--include=refs/remotes/origin')
      --include-regexp REGEXP  process references matching the specified
                               regular expression (e.g.,
                               '--include-regexp=refs/tags/release-.*')
      --exclude PREFIX         don't process references with the specified
                               PREFIX (e.g., '--exclude=refs/notes')
      --exclude-regexp REGEXP  don't process references matching the specified
                               regular expression
      --show-refs              show which refs are being included/excluded

 Prefixes must match at a boundary; for example 'refs/foo' matches
 'refs/foo' and 'refs/foo/bar' but not 'refs/foobar'. Regular
 expression patterns must match the full reference name.

`

var ReleaseVersion string
var BuildVersion string

type NegatedBoolValue struct {
	value *bool
}

func (v *NegatedBoolValue) Set(s string) error {
	b, err := strconv.ParseBool(s)
	*v.value = !b
	return err
}

func (v *NegatedBoolValue) Get() interface{} {
	return !*v.value
}

func (v *NegatedBoolValue) String() string {
	if v == nil || v.value == nil {
		return "true"
	} else {
		return strconv.FormatBool(!*v.value)
	}
}

func (v *NegatedBoolValue) Type() string {
	return "bool"
}

type filterValue struct {
	filter   *git.IncludeExcludeFilter
	polarity git.Polarity
	pattern  string
	regexp   bool
}

func (v *filterValue) Set(s string) error {
	var polarity git.Polarity
	var filter git.ReferenceFilter

	if v.regexp {
		polarity = v.polarity
		var err error
		filter, err = git.RegexpFilter(s)
		if err != nil {
			return fmt.Errorf("invalid regexp: %q", s)
		}
	} else if v.pattern == "" {
		polarity = v.polarity
		filter = git.PrefixFilter(s)
	} else {
		// Allow a boolean value to alter the polarity:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		if b {
			polarity = git.Include
		} else {
			polarity = git.Exclude
		}
		filter = git.PrefixFilter(v.pattern)
	}

	switch polarity {
	case git.Include:
		v.filter.Include(filter)
	case git.Exclude:
		v.filter.Exclude(filter)
	}

	return nil
}

func (v *filterValue) Get() interface{} {
	return nil
}

func (v *filterValue) String() string {
	return ""
}

func (v *filterValue) Type() string {
	if v.regexp {
		return "regexp"
	} else if v.pattern == "" {
		return "prefix"
	} else {
		return ""
	}
}

func main() {
	err := mainImplementation(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
}

func mainImplementation(args []string) error {
	var nameStyle sizes.NameStyle = sizes.NameStyleFull
	var cpuprofile string
	var jsonOutput bool
	var jsonVersion uint
	var threshold sizes.Threshold = 1
	var progress bool
	var version bool
	var filter git.IncludeExcludeFilter
	var showRefs bool

	flags := pflag.NewFlagSet("git-sizer", pflag.ContinueOnError)
	flags.Usage = func() {
		fmt.Print(Usage)
	}

	flags.Var(
		&filterValue{&filter, git.Include, "", false}, "include",
		"include specified references",
	)
	flags.Var(
		&filterValue{&filter, git.Include, "", true}, "include-regexp",
		"include references matching the specified regular expression",
	)
	flags.Var(
		&filterValue{&filter, git.Exclude, "", false}, "exclude",
		"exclude specified references",
	)
	flags.Var(
		&filterValue{&filter, git.Exclude, "", true}, "exclude-regexp",
		"exclude references matching the specified regular expression",
	)

	flag := flags.VarPF(
		&filterValue{&filter, git.Include, "refs/heads/", false}, "branches", "",
		"process all branches",
	)
	flag.NoOptDefVal = "true"

	flag = flags.VarPF(
		&filterValue{&filter, git.Include, "refs/tags/", false}, "tags", "",
		"process all tags",
	)
	flag.NoOptDefVal = "true"

	flag = flags.VarPF(
		&filterValue{&filter, git.Include, "refs/remotes/", false}, "remotes", "",
		"process all remotes",
	)
	flag.NoOptDefVal = "true"

	flags.VarP(
		sizes.NewThresholdFlagValue(&threshold, 0),
		"verbose", "v", "report all statistics, whether concerning or not",
	)
	flags.Lookup("verbose").NoOptDefVal = "true"

	flags.Var(
		&threshold, "threshold",
		"minimum level of concern (i.e., number of stars) that should be\n"+
			"                              reported",
	)

	flags.Var(
		sizes.NewThresholdFlagValue(&threshold, 30),
		"critical", "only report critical statistics",
	)
	flags.Lookup("critical").NoOptDefVal = "true"

	flags.Var(
		&nameStyle, "names",
		"display names of large objects in the specified `style`:\n"+
			"        --names=none            omit footnotes entirely\n"+
			"        --names=hash            show only the SHA-1s of objects\n"+
			"        --names=full            show full names",
	)

	flags.BoolVarP(&jsonOutput, "json", "j", false, "output results in JSON format")
	flags.UintVar(&jsonVersion, "json-version", 1, "JSON format version to output (1 or 2)")

	atty, err := isatty.Isatty(os.Stderr.Fd())
	if err != nil {
		atty = false
	}
	flags.BoolVar(&progress, "progress", atty, "report progress to stderr")
	flags.BoolVar(&showRefs, "show-refs", false, "list the references being processed")
	flags.BoolVar(&version, "version", false, "report the git-sizer version number")
	flags.Var(&NegatedBoolValue{&progress}, "no-progress", "suppress progress output")
	flags.Lookup("no-progress").NoOptDefVal = "true"

	flags.StringVar(&cpuprofile, "cpuprofile", "", "write cpu profile to file")
	flags.MarkHidden("cpuprofile")

	flags.SortFlags = false

	err = flags.Parse(args)
	if err != nil {
		if err == pflag.ErrHelp {
			return nil
		}
		return err
	}

	if jsonOutput && !(jsonVersion == 1 || jsonVersion == 2) {
		return fmt.Errorf("JSON version must be 1 or 2")
	}

	if cpuprofile != "" {
		f, err := os.Create(cpuprofile)
		if err != nil {
			return fmt.Errorf("couldn't set up cpuprofile file: %s", err)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	if version {
		if ReleaseVersion != "" {
			fmt.Printf("git-sizer release %s\n", ReleaseVersion)
		} else {
			fmt.Printf("git-sizer build %s\n", BuildVersion)
		}
		return nil
	}

	if len(flags.Args()) != 0 {
		return errors.New("excess arguments")
	}

	repo, err := git.NewRepository(".")
	if err != nil {
		return fmt.Errorf("couldn't open Git repository: %s", err)
	}
	defer repo.Close()

	var historySize sizes.HistorySize

	var refFilter git.ReferenceFilter = filter.Filter

	if showRefs {
		oldRefFilter := refFilter
		fmt.Fprintf(os.Stderr, "References (included references marked with '+'):\n")
		refFilter = func(refname string) bool {
			b := oldRefFilter(refname)
			if b {
				fmt.Fprintf(os.Stderr, "+ %s\n", refname)
			} else {
				fmt.Fprintf(os.Stderr, "  %s\n", refname)
			}
			return b
		}
	}

	historySize, err = sizes.ScanRepositoryUsingGraph(repo, refFilter, nameStyle, progress)
	if err != nil {
		return fmt.Errorf("error scanning repository: %s", err)
	}

	if jsonOutput {
		var j []byte
		var err error
		switch jsonVersion {
		case 1:
			j, err = json.MarshalIndent(historySize, "", "    ")
		case 2:
			j, err = historySize.JSON(threshold, nameStyle)
		default:
			return fmt.Errorf("JSON version must be 1 or 2")
		}
		if err != nil {
			return fmt.Errorf("could not convert %v to json: %s", historySize, err)
		}
		fmt.Printf("%s\n", j)
	} else {
		io.WriteString(os.Stdout, historySize.TableString(threshold, nameStyle))
	}

	return nil
}
