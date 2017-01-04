// Copyright (c) 2016 Paul Jolly <paul@myitcv.org.uk>, all rights reserved.
// Use of this document is governed by a license found in the LICENSE document.

package main

// gg is a wrapper for ``go generate''. More docs to follow

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	untypedLoopLimit = 10
	typedLoopLimit   = 10
)

var (
	fList    = flag.Bool("l", false, "list directives in packages")
	fVerbose = flag.Bool("v", false, "print the names of packages and files as they are processed")
	fExecute = flag.Bool("x", false, "print commands as they are executed")
	fUntyped = flag.String("untyped", "", "a list of untyped generators to run")
	fTyped   = flag.String("typed", "", "a list of typed generators to run")

	pkgInfo = map[string]*Package{}
)

// Code basically derived from rsc.io/gt
func main() {
	log.SetFlags(0)
	log.SetPrefix("gg: ")

	flag.Parse()

	pkgs := explodePkgList(flag.Args())
	sort.Strings(pkgs)

	wpkgs := pkgs
	diffs := computeStale(wpkgs)

	dirs := calcDirectiveList()
	sort.Strings(dirs)

	if len(dirs) == 0 {
		os.Exit(0)
	}

	if *fList {
		fmt.Println(strings.Join(dirs, "\n"))
		os.Exit(0)
	}

	untypedCmds, typedCmds := validateCmds(dirs, *fUntyped, *fTyped)

	untypedRunExp := buildGoGenRegex(untypedCmds)
	typedRunExp := buildGoGenRegex(typedCmds)

	typedCount := 1

	for {
		untypedCount := 1

		for len(diffs) > 0 {
			if untypedCount > untypedLoopLimit {
				log.Fatalf("Exceeded loop limit for untyped go generate cmd: %v\n", untypedRunExp)
			}

			log.Printf("Untyped run %v\n", untypedCount)
			goGenerate(diffs, untypedRunExp)
			untypedCount++

			diffs = computeStale(diffs)
		}

		// TODO need this largely because of stringer
		// https://github.com/golang/go/issues/10249
		log.Printf("go install\n")
		goInstall(wpkgs)

		if typedCount > typedLoopLimit {
			log.Fatalf("Exceeded loop limit for typed go generate cmd: %v\n", untypedRunExp)
		}

		log.Printf("Typed run %v\n", typedCount)
		goGenerate(wpkgs, typedRunExp)
		typedCount++

		diffs = computeStale(wpkgs)

		if len(diffs) == 0 {
			break
		}

		wpkgs = diffs

	}
}

type Package struct {
	Dir          string
	ImportPath   string
	Standard     bool
	Goroot       bool
	Stale        bool
	GoFiles      []string
	CgoFiles     []string
	CFiles       []string
	CXXFiles     []string
	MFiles       []string
	HFiles       []string
	SFiles       []string
	SwigFiles    []string
	SwigCXXFiles []string
	SysoFiles    []string
	Imports      []string
	Deps         []string
	Incomplete   bool
	Error        *PackageError
	DepsErrors   []*PackageError
	TestGoFiles  []string
	TestImports  []string
	XTestGoFiles []string
	XTestImports []string

	pkgHash string
}

type PackageError struct {
	ImportStack []string
	Pos         string
	Err         string
}

func explodePkgList(pkgs []string) []string {
	out, err := exec.Command("go", append([]string{"list", "-e"}, pkgs...)...).CombinedOutput()
	if err != nil {
		log.Fatalf("go list: %v\n", err)
	}

	return strings.Fields(string(out))
}

func readPkgs(pkgs []string) {
	out, err := exec.Command("go", append([]string{"list", "-e", "-json"}, pkgs...)...).CombinedOutput()
	if err != nil {
		log.Fatalf("go list: %v\n", err)
	}

	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p Package
		if err := dec.Decode(&p); err != nil {
			if err == io.EOF {
				break
			}
			log.Fatalf("reading go list output: %v", err)
		}

		if p.Incomplete {
			// TODO this could probably be improved

			errs := []interface{}{"Error in package ", p.ImportPath, "\n"}
			if p.Error != nil {
				errs = append(errs, p.Error.Err)
			}
			for _, e := range p.DepsErrors {
				errs = append(errs, e.Err)
			}
			log.Fatal(errs...)
		}

		pkgInfo[p.ImportPath] = &p
	}
}

func computeStale(pkgs []string) []string {
	prevHashes := make(map[string]string, len(pkgs))
	for _, p := range pkgs {
		v := ""

		if pkg, ok := pkgInfo[p]; ok {
			v = pkg.pkgHash
		}

		prevHashes[p] = v
	}

	readPkgs(pkgs)

	for _, pkg := range pkgs {
		computePkgHash(pkgInfo[pkg])
	}

	var deltas []string

	for _, p := range pkgs {
		if prevHashes[p] != pkgInfo[p].pkgHash {
			deltas = append(deltas, p)
		}
	}

	return deltas
}

func computePkgHash(p *Package) {
	h := sha1.New()

	fmt.Fprintf(h, "pkg %v\n", p.ImportPath)

	hashFiles(h, p.Dir, p.GoFiles)
	hashFiles(h, p.Dir, p.CgoFiles)
	hashFiles(h, p.Dir, p.CFiles)
	hashFiles(h, p.Dir, p.CXXFiles)
	hashFiles(h, p.Dir, p.MFiles)
	hashFiles(h, p.Dir, p.HFiles)
	hashFiles(h, p.Dir, p.SFiles)
	hashFiles(h, p.Dir, p.SwigFiles)
	hashFiles(h, p.Dir, p.SwigCXXFiles)
	hashFiles(h, p.Dir, p.SysoFiles)
	hashFiles(h, p.Dir, p.TestGoFiles)
	hashFiles(h, p.Dir, p.XTestGoFiles)

	hash := fmt.Sprintf("%x", h.Sum(nil))
	p.pkgHash = hash
}

func hashFiles(h io.Writer, dir string, files []string) {
	for _, file := range files {
		f, err := os.Open(filepath.Join(dir, file))
		if err != nil {
			log.Fatalf("hashFiles: %v\n", err)
		}

		fmt.Fprintf(h, "file %s\n", file)
		n, _ := io.Copy(h, f)
		fmt.Fprintf(h, "%d bytes\n", n)

		f.Close()
	}
}

func splitCmdList(s string) []string {
	s = strings.TrimSpace(s)
	ps := strings.Split(s, ",")

	parts := make([]string, 0, len(ps))

	for _, v := range ps {
		v = strings.TrimSpace(v)

		if v == "" {
			continue
		}

		if !validCmd.MatchString(v) {
			log.Fatalf("Invalid go generate cmd: %v\n", v)
		}

		parts = append(parts, v)
	}

	return parts
}

func validateCmds(dirs []string, untypedList, typedList string) ([]string, []string) {
	ntcmds := splitCmdList(untypedList)
	tcmds := splitCmdList(typedList)

	dmap := make(map[string]bool)
	ntmap := make(map[string]struct{})
	tmap := make(map[string]struct{})

	for _, v := range dirs {
		dmap[v] = false
	}

	for _, v := range ntcmds {
		if _, ok := dmap[v]; !ok {
			log.Fatalf("Directive provided but does not appear in any files: %q\n", v)
		}

		dmap[v] = true
		ntmap[v] = struct{}{}
	}

	for _, v := range tcmds {
		if _, ok := dmap[v]; !ok {
			log.Fatalf("Directive provided but does not appear in any files: %v\n", v)
		}

		if _, ok := ntmap[v]; ok {
			log.Fatalf("Directive provided as both typed and untyped: %v\n", v)
		}

		dmap[v] = true
		tmap[v] = struct{}{}
	}

	for k, v := range dmap {
		if !v {
			log.Fatalf("Directive appears in source files but not categorised as either typed and untyped: %v\n", k)
		}
	}

	// now de-dup just for completeness
	ntcmds = make([]string, 0, len(ntmap))
	tcmds = make([]string, 0, len(tmap))

	for k := range ntmap {
		ntcmds = append(ntcmds, k)
	}

	for k := range tmap {
		tcmds = append(tcmds, k)
	}

	return ntcmds, tcmds
}

// TODO this is probably too restrictive
var validCmd = regexp.MustCompile(`^[a-zA-Z0-9]+$`)

func buildGoGenRegex(parts []string) string {
	escpd := make([]string, len(parts))

	for i := range parts {
		escpd[i] = regexp.QuoteMeta(parts[i])
	}

	exp := fmt.Sprintf("//go:generate (?:%v)(?:$| )", strings.Join(escpd, "|"))

	// aggressively ensure the regexp compiles here... else a call to go generate
	// will be useless
	_, err := regexp.Compile(exp)
	if err != nil {
		log.Fatalf("Could not form valid go generate command: %v\n", err)
	}

	return exp
}

func goGenerate(pkgs []string, runExp string) {
	args := []string{"generate"}

	if *fVerbose {
		args = append(args, "-v")
	}

	if *fExecute {
		args = append(args, "-x")
	}

	args = append(args, "-run", runExp)

	args = append(args, pkgs...)

	out, err := exec.Command("go", args...).CombinedOutput()
	if err != nil {
		log.Fatalf("go generate: %v\n%s", err, out)
	}

	if len(out) > 0 {
		fmt.Print(string(out))
	}
}

func goInstall(pkgs []string) {
	args := append([]string{"install"}, pkgs...)
	// TODO is there anything better we can do than simply ignore the exit status of go install?
	// because things are in a partial state
	out, _ := exec.Command("go", args...).CombinedOutput()

	if *fExecute {
		c := make([]interface{}, 1, len(args)+1)

		c[0] = "go"

		for _, v := range args {
			c = append(c, v)
		}

		fmt.Println(c...)
		if len(out) > 0 {
			fmt.Printf(string(out))
		}
	}
}

func calcDirectiveList() []string {
	res := make(map[string]struct{})

	for _, v := range pkgInfo {

		var goFiles []string
		goFiles = append(goFiles, v.GoFiles...)
		goFiles = append(goFiles, v.CgoFiles...)
		goFiles = append(goFiles, v.TestGoFiles...)
		goFiles = append(goFiles, v.XTestGoFiles...)

		for _, f := range goFiles {

			f = filepath.Join(v.Dir, f)

			of, err := os.Open(f)
			if err != nil {
				log.Fatalf("calc gen list: %v\n", err)
			}

			// largely borrowed from cmd/go/generate.go

			input := bufio.NewReader(of)
			line := 0
			for {
				line++

				var buf []byte
				buf, err = input.ReadSlice('\n')
				if err == bufio.ErrBufferFull {
					// Line too long - consume and ignore.
					if isGoGenerate(buf) {
						log.Printf("calc gen list: directive too long in on line %v in %v\n", line, f)
					}

					for err == bufio.ErrBufferFull {
						_, err = input.ReadSlice('\n')
					}

					if err != nil {
						log.Fatalf("calc gen list: failed to recover from long line: %v\n", err)
					}

					// we recovered...
					continue
				}

				if err == io.EOF {
					break
				}

				if err != nil {
					log.Fatalf("calc gen list: %v\n", err)
				}

				if isGoGenerate(buf) {
					parts := strings.Fields(string(buf))

					if len(parts) < 2 {
						log.Fatalf("calc gen list: no arguments to direct on line %v in %v\n", line, f)
					}

					res[parts[1]] = struct{}{}
				}
			}

			of.Close()
		}
	}

	ps := make([]string, 0, len(res))

	for k := range res {
		ps = append(ps, k)
	}

	return ps
}

// borrowed from cmd/go/generate.go
func isGoGenerate(buf []byte) bool {
	return bytes.HasPrefix(buf, []byte("//go:generate ")) || bytes.HasPrefix(buf, []byte("//go:generate\t"))
}
