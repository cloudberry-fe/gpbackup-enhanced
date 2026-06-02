package tocextract

import (
	"bufio"
	"bytes"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Minimal but representative TOC. Uses the shape yaml.v2 produces:
// dataentries block in plain (unquoted) style, plus the noise fields
// (attributestring with quotes, rowscopied, etc.) so the scanner has
// to skip non-key lines correctly.
const sampleTOC = `globalentries: []
predataentries: []
postdataentries: []
statisticsentries: []
incrementalmetadata:
  ao: {}
  heap: {}
dataentries:
- schema: public
  name: t1
  oid: 16001
  attributestring: '"c1" int, "c2" text'
  rowscopied: 100
  partitionroot: ""
  isreplicated: false
  distbyenum: false
- schema: etl
  name: t2
  oid: 16002
  attributestring: '"x" bigint'
  rowscopied: 0
  partitionroot: ""
  isreplicated: false
  distbyenum: false
- schema: etl
  name: t3
  oid: 16003
  attributestring: '"y" date'
  rowscopied: 50
  partitionroot: ""
  isreplicated: false
  distbyenum: false
`

func writeSample(t *testing.T) string {
	t.Helper()
	dir, err := ioutil.TempDir("", "tocextract")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	p := filepath.Join(dir, "toc.yaml")
	if err := ioutil.WriteFile(p, []byte(sampleTOC), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func runAndGet(t *testing.T, opts Options) string {
	t.Helper()
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	if err := Run(opts, w); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestFastMatchesStrict(t *testing.T) {
	p := writeSample(t)
	fast := runAndGet(t, Options{TOCPath: p, Strict: false})
	strict := runAndGet(t, Options{TOCPath: p, Strict: true})
	if fast != strict {
		t.Fatalf("fast vs strict differ:\nFAST:\n%s\nSTRICT:\n%s", fast, strict)
	}
	want := "public|t1|16001\netl|t2|16002\netl|t3|16003\n"
	if fast != want {
		t.Fatalf("unexpected output:\n%s\nwant:\n%s", fast, want)
	}
}

func TestIncludeTableFilter(t *testing.T) {
	p := writeSample(t)
	opts := Options{
		TOCPath:       p,
		IncludeTables: map[string]bool{"etl.t2": true},
	}
	out := runAndGet(t, opts)
	if out != "etl|t2|16002\n" {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestIncludeSchemaFilter(t *testing.T) {
	p := writeSample(t)
	opts := Options{
		TOCPath:        p,
		IncludeSchemas: map[string]bool{"etl": true},
	}
	out := runAndGet(t, opts)
	want := "etl|t2|16002\netl|t3|16003\n"
	if out != want {
		t.Fatalf("unexpected output: %q want %q", out, want)
	}
}

func TestStopAtNextTopLevelKey(t *testing.T) {
	// Trailing top-level key shouldn't cause spurious entries.
	const trailer = sampleTOC + `incrementalmetadata:
  ao:
    public.fooz:
      modcount: 1
      lastddltimestamp: "x"
`
	dir, err := ioutil.TempDir("", "tocextract")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	p := filepath.Join(dir, "toc.yaml")
	if err := ioutil.WriteFile(p, []byte(trailer), 0644); err != nil {
		t.Fatal(err)
	}
	out := runAndGet(t, Options{TOCPath: p})
	got := strings.Count(out, "\n")
	if got != 3 {
		t.Fatalf("expected 3 entries, got %d:\n%s", got, out)
	}
}

func TestQuotedScalarStripped(t *testing.T) {
	// yaml.v2 may quote values containing colons or starting with special
	// chars. The scanner must strip the surrounding quotes.
	const quoted = `dataentries:
- schema: 'public'
  name: "weird:name"
  oid: 42
`
	dir, err := ioutil.TempDir("", "tocextract")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	p := filepath.Join(dir, "toc.yaml")
	if err := ioutil.WriteFile(p, []byte(quoted), 0644); err != nil {
		t.Fatal(err)
	}
	out := runAndGet(t, Options{TOCPath: p})
	if out != "public|weird:name|42\n" {
		t.Fatalf("scalar stripping failed: %q", out)
	}
}
