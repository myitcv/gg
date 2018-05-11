package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"go/build"
	"hash"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"myitcv.io/gogenerate"
)

var (
	pkgInfo = map[string]*Package{}
)

type Package struct {
	Dir          string
	Name         string
	ImportPath   string
	Target       string
	Deps         []string
	GoFiles      []string
	CgoFiles     []string
	TestGoFiles  []string
	XTestGoFiles []string

	// toolDeps maps the import path of a tool dep
	// to a slice of directories that represent the
	// additional output paths of the tool (Dir is
	// assumed)
	deps     map[string]bool
	toolDeps map[string]map[string]bool

	// rdeps are _both_ the reverse deps and toolDeps
	// (because we don't need to distinguish in this
	// direction)
	rdeps map[string]bool

	pkgHash string
}

type pkg struct {
	Dir        string
	Name       string
	ImportPath string
	Target     string

	// used for hashing
	GoFiles  []string
	CgoFiles []string

	testPkg   *pkg
	isTestPkg bool

	deps     map[*pkg]bool
	toolDeps map[*pkg]map[string]bool

	rdeps map[*pkg]bool

	inPkgSpec bool

	isTool  bool
	pending *int

	hash []byte
}

func (p *pkg) String() string {
	return p.ImportPath
}

func (p *pkg) rehash() {
	if p.hash != nil {
		return
	}
	h := sha256.New()
	// when we enable full loading of deps this distinction will
	// go away
	if p.inPkgSpec {
		var deps []*pkg
		for d := range p.deps {
			if d.inPkgSpec {
				deps = append(deps, d)
			}
		}
		for t := range p.toolDeps {
			deps = append(deps, t)
		}
		sort.Slice(deps, func(i, j int) bool {
			return deps[i].ImportPath < deps[j].ImportPath
		})
		for _, d := range deps {
			d.rehash()
			if _, err := h.Write(d.hash); err != nil {
				fatalf("failed to hash: %v", err)
			}
		}
		p.hashFiles(h, p.GoFiles)
		p.hashFiles(h, p.CgoFiles)
	}
	p.hash = h.Sum(nil)
}

func (p *pkg) hashFiles(h hash.Hash, files []string) {
	for _, f := range files {
		path := filepath.Join(p.Dir, f)
		fi, err := os.Open(path)
		if err != nil {
			fatalf("failed to open %v: %v", path, err)
		}
		_, err = io.Copy(h, fi)
		fi.Close()
		if err != nil {
			fatalf("failed to hash %v: %v", path, err)
		}
	}
}

type pkgSet map[*pkg]bool

// returns the tools detected during scanning of the loaded packages
func readPkgs(pkgs []string, loaded pkgSet, dontScan bool) pkgSet {
	if len(pkgs) == 0 {
		return nil
	}

	tools := make(pkgSet)
	res := make(chan *Package)

	go func() {
		args := []string{"go", "list", "-test", "-f", `{"Dir": "{{.Dir}}", "Name": "{{.Name}}", "ImportPath": "{{.ImportPath}}"{{if eq .Name "main"}}, "Target": "{{.Target}}"{{end}}{{with .Deps}}, "Deps": ["{{join . "\", \""}}"]{{end}}{{with .GoFiles}}, "GoFiles": ["{{join . "\", \""}}"]{{end}}{{with .TestGoFiles}}, "TestGoFiles": ["{{join . "\", \""}}"]{{end}}{{with .CgoFiles}}, "CgoFiles": ["{{join . "\", \""}}"]{{end}}{{with .XTestGoFiles}}, "XTestGoFiles": ["{{join . "\", \""}}"]{{end}}}`}
		args = append(args, pkgs...)
		cmd := exec.Command(args[0], args[1:]...)

		out, err := cmd.CombinedOutput()
		if err != nil {
			fatalf("failed to run %v: %v", strings.Join(cmd.Args, " "))
		}

		dec := json.NewDecoder(bytes.NewReader(out))
		for {
			var p Package
			if err := dec.Decode(&p); err != nil {
				if err == io.EOF {
					break
				}
				fatalf("failed to decode output from golist: %v\n%s", err, out)
			}
			res <- &p
		}
		close(res)
	}()

	for pp := range res {
		// we collapse down the test deps into the package deps
		// because from a go generate perspective they are one and
		// the same. We don't care for the files in the test

		p := resolve(pp.ImportPath)
		p.Dir = pp.Dir
		p.Name = pp.Name
		p.Target = pp.Target

		p.GoFiles = pp.GoFiles
		p.CgoFiles = pp.CgoFiles

		p.inPkgSpec = true
		loaded[p] = true

		// invalidate any existing hash
		p.hash = nil

		ip := pp.ImportPath
		if strings.HasSuffix(ip, ".test") {
			p.isTestPkg = true
			ip = strings.TrimSuffix(ip, ".test")
			rp := resolve(ip)
			rp.testPkg = p
			continue
		}

		p.deps = make(pkgSet)

		for _, d := range pp.Deps {
			p.deps[resolve(d)] = true
		}

		if dontScan {
			continue
		}

		var gofiles []string
		gofiles = append(gofiles, pp.GoFiles...)
		gofiles = append(gofiles, pp.CgoFiles...)
		gofiles = append(gofiles, pp.TestGoFiles...)
		gofiles = append(gofiles, pp.XTestGoFiles...)

		dirs := make(map[*pkg]map[string]bool)

		for _, f := range gofiles {
			check := func(line int, args []string) error {
				// TODO add support for go run with package

				cmd := args[0]
				cmdPath, ok := config.baseCmds[cmd]
				if !ok {
					return fmt.Errorf("failed to resolve cmd %v", cmd)
				}
				cmdPkg := resolve(cmdPath)
				pm, ok := dirs[cmdPkg]
				if !ok {
					pm = make(map[string]bool)
					dirs[cmdPkg] = pm
					cmdPkg.isTool = true
					tools[cmdPkg] = true
				}

				for i, a := range args {
					if a == "--" {
						// end of flags
						break
					}
					const prefix = "-" + gogenerate.FlagOutPkgPrefix
					if !strings.HasPrefix(a, prefix) {
						continue
					}

					rem := strings.TrimPrefix(a, prefix)
					if len(rem) == 0 || rem[0] == '=' || rem[0] == '-' {
						return fmt.Errorf("bad arg %v", a)
					}

					var dirOrPkg string
					var pkgPath string

					for j := 1; j < len(rem); j++ {
						if rem[j] == '=' {
							dirOrPkg = rem[j+1:]
							goto ResolveDirOrPkg
						}
					}

					if i+1 == len(args) {
						return fmt.Errorf("bad args %q", strings.Join(args, " "))
					} else {
						dirOrPkg = args[i+1]
					}

				ResolveDirOrPkg:
					// TODO we could improve this logic
					if filepath.IsAbs(dirOrPkg) {
						bpkg, err := build.ImportDir(dirOrPkg, build.FindOnly)
						if err != nil {
							return fmt.Errorf("failed to resolve %v to a package: %v", dirOrPkg, err)
						}
						pkgPath = bpkg.ImportPath
					} else {
						bpkg, err := build.Import(dirOrPkg, p.Dir, build.FindOnly)
						if err != nil {
							return fmt.Errorf("failed to resolve %v in %v to a package: %v", dirOrPkg, p.Dir, err)
						}
						pkgPath = bpkg.ImportPath
					}

					pm[pkgPath] = true
				}

				return nil
			}
			if err := gogenerate.DirFunc(ip, p.Dir, f, check); err != nil {
				fatalf("error checking %v: %v", filepath.Join(p.Dir, f), err)
			}
		}

		for d, ods := range dirs {
			if p.toolDeps == nil {
				p.toolDeps = make(map[*pkg]map[string]bool)
			}
			p.toolDeps[d] = ods

			// verify that none of the output directories are a Dep
			for op := range ods {
				if p.deps[resolve(op)] {
					fatalf("package %v has directive %v that outputs to %v, but that is also a dep", p.ImportPath, d, op)
				}
			}
		}

		for _, f := range gofiles {
			gd, ok := gogenerate.FileIsGenerated(f)

			// TODO improve hack for protobuf generated files
			if !ok || strings.HasSuffix(f, ".pb.go") {
				continue
			}

			found := false

			for d := range dirs {
				if path.Base(d.ImportPath) == gd {
					found = true
					break
				}
			}

			// TODO implement delete if we are not in list mode
			if !found {
				fmt.Printf(">> will delete %v\n", filepath.Join(p.Dir, f))
			}
		}

		// ====================
		// DEBUG OUTPUT

		fmt.Printf("%v\n", p.ImportPath)

		var ds []string
		for d := range p.deps {
			ds = append(ds, d.ImportPath)
		}
		sort.Strings(ds)
		for _, d := range ds {
			fmt.Printf(" d - %v\n", d)
		}
		for t, dirs := range p.toolDeps {
			ods := ""
			if len(dirs) != 0 {
				var odss []string
				for od := range dirs {
					odss = append(odss, od)
				}
				sort.Strings(odss)
				ods = fmt.Sprintf(" [%v]", strings.Join(odss, ","))
			}
			fmt.Printf(" t - %v%v\n", t, ods)
		}
	}

	return tools
}

// func snapHash(pkgs []string) map[string]string {
// 	prevHashes := make(map[string]string, len(pkgs))
// 	for _, p := range pkgs {
// 		v := ""

// 		if pkg, ok := pkgInfo[p]; ok {
// 			v = pkg.pkgHash
// 		}

// 		prevHashes[p] = v
// 	}

// 	return prevHashes
// }

// func computeStale(pkgs []string, read bool) []string {
// 	snap := snapHash(pkgs)

// 	if read {
// 		readPkgs(pkgs, false)
// 	}

// 	for _, pkg := range pkgs {
// 		computePkgHash(pkgInfo[pkg])
// 	}

// 	return deltaHash(snap)
// }

// func deltaHash(snap map[string]string) []string {
// 	var deltas []string

// 	for p := range snap {
// 		if snap[p] != pkgInfo[p].pkgHash {
// 			deltas = append(deltas, p)
// 		}
// 	}

// 	return deltas
// }

// func computePkgHash(p *Package) {
// 	h := sha1.New()

// 	fmt.Fprintf(h, "pkg %v\n", p.ImportPath)

// 	hashFiles(h, p.Dir, p.GoFiles)
// 	hashFiles(h, p.Dir, p.CgoFiles)
// 	hashFiles(h, p.Dir, p.CFiles)
// 	hashFiles(h, p.Dir, p.CXXFiles)
// 	hashFiles(h, p.Dir, p.MFiles)
// 	hashFiles(h, p.Dir, p.HFiles)
// 	hashFiles(h, p.Dir, p.SFiles)
// 	hashFiles(h, p.Dir, p.SwigFiles)
// 	hashFiles(h, p.Dir, p.SwigCXXFiles)
// 	hashFiles(h, p.Dir, p.SysoFiles)
// 	hashFiles(h, p.Dir, p.TestGoFiles)
// 	hashFiles(h, p.Dir, p.XTestGoFiles)

// 	hash := fmt.Sprintf("%x", h.Sum(nil))
// 	p.pkgHash = hash
// }

// func hashFiles(h io.Writer, dir string, files []string) {
// 	for _, file := range files {
// 		fn := filepath.Join(dir, file)
// 		f, err := os.Open(fn)
// 		if err != nil {
// 			fatalf("could not open file %v: %v\n", fn, err)
// 		}

// 		fmt.Fprintf(h, "file %s\n", file)
// 		n, _ := io.Copy(h, f)
// 		fmt.Fprintf(h, "%d bytes\n", n)

// 		f.Close()
// 	}
// }
