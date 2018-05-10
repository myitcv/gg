// Copyright (c) 2016 Paul Jolly <paul@myitcv.org.uk>, all rights reserved.
// Use of this document is governed by a license found in the LICENSE document.

package main // import "myitcv.io/gg"

// gg is a wrapper for ``go generate''. More docs to follow

import (
	"flag"
	"fmt"
	"go/build"
	"log"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"myitcv.io/gogenerate"
)

const (
	loopLimit      = 10
	typedLoopLimit = loopLimit

	debug = false
)

var (
	fDebug = flag.Bool("debug", false, "debug logging")
)

// All code basically derived from rsc.io/gt

func main() {
	setupAndParseFlags("")

	loadConfig()

	pkgs := make(map[string]*pkg)

	resolve := func(ip string) *pkg {
		p, ok := pkgs[ip]
		if ok {
			return p
		}

		p = &pkg{
			ImportPath: ip,
		}
		pkgs[ip] = p
		return p
	}

	for pp := range readPkgs(flag.Args()) {
		// we collapse down the test deps into the package deps
		// because from a go generate perspective they are one and
		// the same. We don't care for the files in the test

		p := resolve(pp.ImportPath)
		p.Dir = pp.Dir
		p.Name = pp.Name
		p.inPkgSpec = true

		ip := pp.ImportPath
		if strings.HasSuffix(ip, ".test") {
			ip = strings.TrimSuffix(ip, ".test")
			rp := resolve(ip)
			rp.testPkg = p
			continue
		}

		p.deps = make(pkgSet)

		for _, d := range pp.Deps {
			p.deps[resolve(d)] = true
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

		pkgs[p.ImportPath] = p
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

	// will all be tools
	possRoots := make(pkgSet)

	// populate rdeps
	// TODO we don't need to fully populate this so look to trim at
	// some point later
	for _, p := range pkgs {
		p.pending = len(p.toolDeps)
		for d := range p.deps {
			if d.rdeps == nil {
				d.rdeps = make(pkgSet)
			}
			d.rdeps[p] = true
			if len(d.toolDeps) > 0 {
				p.pending++
			}
		}
		for t := range p.toolDeps {
			if t.rdeps == nil {
				t.rdeps = make(pkgSet)
			}
			t.rdeps[p] = true
		}
		if p.isTool && !p.inPkgSpec || p {
			possRoots[p] = true
		}
	}

	// fmt.Printf("Possible roots: %v\n", strings.Join(importPaths(possRoots), ", "))

	for pr := range possRoots {
		fmt.Printf("Poss root: %v\n", pr)
		for rd := range pr.rdeps {
			fmt.Printf(" - %v\n", rd)
		}
	}

	// var work []*pkg
	// for pr := range possRoots {
	// 	work = append(work, pr)
	// }

	// var h *pkg

	// for len(work) > 0 {
	// 	h, work = work[0], work[1:]
	// 	fmt.Printf("%v\n", h)
	// 	for rd := range h.rdeps {
	// 		rd.pending++
	// 		fmt.Printf(" - %v %v\n", rd.pending, rd)
	// 		if len(rd.rdeps) > 0 {
	// 			work = append(work, rd)
	// 		}
	// 	}
	// }
	// 	if h.done {
	// 		continue
	// 	}

	// 	h.done = true

	// 	for rd := range h.rdeps {
	// 		if rd.done {
	// 			continue
	// 		}
	// 	}
	// }
}

func importPaths(ps pkgSet) []string {
	var vs []string
	for p := range ps {
		vs = append(vs, p.ImportPath)
	}
	sort.Strings(vs)
	return vs

}

func xlog(args ...interface{}) {
	if *fVVerbose || *fExecute {
		log.Print(args...)
	}
}

func xlogf(format string, args ...interface{}) {
	if *fVVerbose || *fExecute {
		log.Print(args...)
	}
}

func vvlogf(format string, args ...interface{}) {
	if *fVVerbose {
		log.Printf(format, args...)
	}
}

func cmdMap(cmds map[string]map[string]struct{}) map[string]struct{} {
	allCmds := make(map[string]struct{})

	for _, m := range cmds {
		for k := range m {
			allCmds[k] = struct{}{}
		}
	}

	return allCmds
}
