// Package tocextract emits `schema|name|oid` triples for every entry
// under `dataentries:` in a gpbackup TOC YAML file. Two parsers are
// available:
//
//   - line scanner (default) — matches the deterministic layout that
//     gpbackup writes via gopkg.in/yaml.v2; sub-second on 60 MB files.
//   - strict (-strict) — full yaml.Unmarshal into a slim struct; used
//     as a correctness oracle and as a fallback if the layout assumption
//     ever breaks.
//
// Designed to replace the python2 yaml.safe_load path inside
// gpbackup_ext_query.sh, which is O(minutes) on large TOCs.
package tocextract

import (
	"bufio"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v2"
)

// stringSetFlag collects --include-table / --include-schema repeats.
type stringSetFlag map[string]bool

func (s *stringSetFlag) String() string { return fmt.Sprintf("%v", *s) }
func (s *stringSetFlag) Set(v string) error {
	if *s == nil {
		*s = make(map[string]bool)
	}
	(*s)[v] = true
	return nil
}

// Options groups parsed CLI args so tests can exercise the same code
// path without rebuilding global flag state.
type Options struct {
	TOCPath        string
	Strict         bool
	IncludeTables  map[string]bool // "schema.name" → present
	IncludeSchemas map[string]bool // "schema"      → present
}

// DoTocExtract is the binary entry point.
func DoTocExtract() {
	fs := flag.NewFlagSet("gpbackup_toc_extract", flag.ExitOnError)
	tocPath := fs.String("toc", "", "path to gpbackup_<TS>_toc.yaml (required)")
	strict := fs.Bool("strict", false, "use yaml.Unmarshal (slower, format-tolerant); default is the line scanner")
	var tables, schemas stringSetFlag
	fs.Var(&tables, "include-table", "only emit this schema.table (repeat to add more)")
	fs.Var(&schemas, "include-schema", "only emit tables in this schema (repeat to add more)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), `Usage: gpbackup_toc_extract --toc <path> [--strict] [--include-table s.t]... [--include-schema s]...

Streams a gpbackup TOC YAML and prints "schema|name|oid" for each entry
under dataentries:. Output is suitable for piping into shell loops.

If no --include-* filters are given, all entries are emitted.`)
		fs.PrintDefaults()
	}
	_ = fs.Parse(os.Args[1:])
	if *tocPath == "" {
		fs.Usage()
		os.Exit(2)
	}
	opts := Options{
		TOCPath:        *tocPath,
		Strict:         *strict,
		IncludeTables:  tables,
		IncludeSchemas: schemas,
	}
	w := bufio.NewWriterSize(os.Stdout, 1<<20)
	if err := Run(opts, w); err != nil {
		_ = w.Flush()
		fmt.Fprintf(os.Stderr, "gpbackup_toc_extract: %v\n", err)
		os.Exit(1)
	}
	_ = w.Flush()
}

// Run dispatches to the requested parser. Tests call this directly.
func Run(opts Options, out *bufio.Writer) error {
	if opts.Strict {
		return runStrict(opts, out)
	}
	return runFast(opts, out)
}

// keep emits a record only when filters allow it.
func keep(opts Options, schema, name string) bool {
	if len(opts.IncludeTables) > 0 && !opts.IncludeTables[schema+"."+name] {
		return false
	}
	if len(opts.IncludeSchemas) > 0 && !opts.IncludeSchemas[schema] {
		return false
	}
	return true
}

// stripScalar removes surrounding single/double quotes and trims spaces;
// gpbackup's yaml.v2 emits plain scalars for these fields, but be safe.
func stripScalar(s string) string {
	s = strings.TrimSpace(s)
	if n := len(s); n >= 2 {
		if (s[0] == '\'' && s[n-1] == '\'') || (s[0] == '"' && s[n-1] == '"') {
			s = s[1 : n-1]
		}
	}
	return s
}

// ── Fast path: line scanner ─────────────────────────────────────────
//
// gpbackup writes dataentries with this exact shape (yaml.v2 plain block):
//
//   dataentries:
//   - schema: public
//     name: my_table
//     oid: 16384
//     attributestring: '...'
//     rowscopied: 0
//     partitionroot: ""
//     isreplicated: false
//     distbyenum: false
//
// We seek to the literal line "dataentries:", then read line-by-line until
// a left-margin line that isn't part of the list (i.e. another top-level
// key). Each new entry starts with "- schema:".

func runFast(opts Options, out *bufio.Writer) error {
	f, err := os.Open(opts.TOCPath)
	if err != nil {
		return err
	}
	defer f.Close()

	br := bufio.NewReaderSize(f, 4*1024*1024)
	var (
		inData            bool
		schema, name, oid string
	)

	flush := func() {
		if schema != "" && name != "" && oid != "" && keep(opts, schema, name) {
			fmt.Fprintf(out, "%s|%s|%s\n", schema, name, oid)
		}
		schema, name, oid = "", "", ""
	}

	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			trim := strings.TrimRight(line, "\n\r")
			if !inData {
				if trim == "dataentries:" {
					inData = true
				}
			} else {
				switch {
				case strings.HasPrefix(trim, "- schema:"):
					flush()
					schema = stripScalar(trim[len("- schema:"):])
				case strings.HasPrefix(trim, "  name:"):
					name = stripScalar(trim[len("  name:"):])
				case strings.HasPrefix(trim, "  oid:"):
					oid = stripScalar(trim[len("  oid:"):])
				default:
					// A line that starts at column 0 and isn't "- ..." means
					// the dataentries block ended.
					if len(trim) > 0 && trim[0] != ' ' && trim[0] != '-' {
						flush()
						inData = false
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	flush()
	return nil
}

// ── Strict path: yaml.Unmarshal ─────────────────────────────────────
//
// Functional oracle. Decodes into a slim struct that ignores every
// field except the three we need.

type strictEntry struct {
	Schema string `yaml:"schema"`
	Name   string `yaml:"name"`
	Oid    uint64 `yaml:"oid"`
}

type strictTOC struct {
	DataEntries []strictEntry `yaml:"dataentries"`
}

func runStrict(opts Options, out *bufio.Writer) error {
	data, err := ioutil.ReadFile(opts.TOCPath)
	if err != nil {
		return err
	}
	var toc strictTOC
	if err := yaml.Unmarshal(data, &toc); err != nil {
		return err
	}
	for _, e := range toc.DataEntries {
		if !keep(opts, e.Schema, e.Name) {
			continue
		}
		fmt.Fprintf(out, "%s|%s|%s\n", e.Schema, e.Name, strconv.FormatUint(e.Oid, 10))
	}
	return nil
}
