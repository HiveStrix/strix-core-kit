// Command capcheck is the CI check of a Core's operation catalog
// (capabilities/catalog.yaml, contract AI ready §1.4). It fails when:
//
//   - the catalog is malformed (unknown field, missing field, effect or risk
//     outside its enum, id that is not its action, rpc/id/action twice);
//   - an RPC of the Core's services is not catalogued;
//   - a catalogued rpc is not declared by any proto, or its request or
//     response is not the RPC's;
//   - with --module, an action whose first segment is not one of them.
//
// Usage, from the Core's repo root:
//
//	go run github.com/hs-javierviquez/strix-core-kit/cmd/capcheck@<kit version> \
//	    --catalog capabilities/catalog.yaml --proto proto \
//	    --package billing.v1 --module billing
//
// --proto and --package and --module repeat. --package limits the check to
// the services of those proto packages: a Core that keeps synced copies of
// other contracts under proto/ (a client stub, the PDP's) must not be asked
// to catalogue someone else's RPCs. Without --package every service found
// under the --proto dirs is the Core's.
//
// Protos are parsed, not compiled: imports are never resolved, so the check
// needs neither buf nor the googleapis dependency. A type written relative
// to the package is resolved against the messages of the scanned files; one
// declared elsewhere (google.protobuf.Empty) is taken as written.
//
// Exit code 0 is clean, 1 is a catalog problem (every one is printed), 2 is
// a usage or I/O error.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bufbuild/protocompile/parser"
	"github.com/bufbuild/protocompile/reporter"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/hs-javierviquez/strix-core-kit/capabilities"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func run(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("capcheck", flag.ContinueOnError)
	fl.SetOutput(stderr)
	var protoDirs, packages, modules multi
	catalogPath := fl.String("catalog", "capabilities/catalog.yaml", "path of the operation catalog")
	fl.Var(&protoDirs, "proto", "directory with the Core's .proto files (repeatable)")
	fl.Var(&packages, "package", "proto package whose services are the Core's (repeatable; default: all)")
	fl.Var(&modules, "module", "module an action may belong to (repeatable; default: not checked)")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if len(protoDirs) == 0 {
		fmt.Fprintln(stderr, "capcheck: at least one --proto directory is required")
		return 2
	}

	cat, err := capabilities.Load(*catalogPath)
	if err != nil {
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			fmt.Fprintln(stderr, err)
			return 2
		}
		fmt.Fprintln(stderr, err)
		return 1
	}
	rpcs, err := scan(protoDirs, packages)
	if err != nil {
		fmt.Fprintln(stderr, "capcheck:", err)
		return 2
	}

	var problems []string
	if err := cat.CheckRPCs(rpcs); err != nil {
		problems = append(problems, err.Error())
	}
	if len(modules) > 0 {
		if err := cat.CheckModules(modules...); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if len(problems) > 0 {
		fmt.Fprintf(stderr, "capcheck: %s\n", *catalogPath)
		fmt.Fprintln(stderr, strings.Join(problems, "\n"))
		return 1
	}
	fmt.Fprintf(stdout, "capcheck: %s OK, %d operations, %d rpcs\n", *catalogPath, len(cat.Entries), len(rpcs))
	return 0
}

// scan parses every .proto under dirs and returns the RPCs of the services
// in packages (all, when packages is empty), sorted by name.
func scan(dirs, packages []string) ([]capabilities.RPC, error) {
	var files []*descriptorpb.FileDescriptorProto
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || filepath.Ext(path) != ".proto" {
				return nil
			}
			fd, err := parseFile(path)
			if err != nil {
				return err
			}
			files = append(files, fd)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	known := map[string]bool{}
	for _, f := range files {
		for _, m := range f.GetMessageType() {
			collectMessages(known, f.GetPackage(), m)
		}
	}
	want := map[string]bool{}
	for _, p := range packages {
		want[p] = true
	}

	var out []capabilities.RPC
	for _, f := range files {
		pkg := f.GetPackage()
		if len(want) > 0 && !want[pkg] {
			continue
		}
		for _, svc := range f.GetService() {
			for _, m := range svc.GetMethod() {
				out = append(out, capabilities.RPC{
					Name:     qualify(pkg, svc.GetName()) + "/" + m.GetName(),
					Request:  resolve(known, pkg, m.GetInputType()),
					Response: resolve(known, pkg, m.GetOutputType()),
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func parseFile(path string) (*descriptorpb.FileDescriptorProto, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := reporter.NewHandler(nil)
	node, err := parser.Parse(path, f, h)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	res, err := parser.ResultFromAST(node, true, h)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return res.FileDescriptorProto(), nil
}

func collectMessages(known map[string]bool, scope string, m *descriptorpb.DescriptorProto) {
	name := qualify(scope, m.GetName())
	known[name] = true
	for _, n := range m.GetNestedType() {
		collectMessages(known, name, n)
	}
}

func qualify(scope, name string) string {
	if scope == "" {
		return name
	}
	return scope + "." + name
}

// resolve turns a type reference as written in an unlinked file into a fully
// qualified name, following protobuf's scoping: innermost package scope
// first, then outwards. A reference that matches no scanned message is
// declared in an import the check does not read; it is taken as written,
// qualified with the file's package only when it has no dot at all.
func resolve(known map[string]bool, pkg, ref string) string {
	if strings.HasPrefix(ref, ".") {
		return ref[1:]
	}
	scope := pkg
	for {
		if c := qualify(scope, ref); known[c] {
			return c
		}
		if scope == "" {
			break
		}
		if i := strings.LastIndex(scope, "."); i >= 0 {
			scope = scope[:i]
		} else {
			scope = ""
		}
	}
	if strings.Contains(ref, ".") {
		return ref
	}
	return qualify(pkg, ref)
}
