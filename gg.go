// Copyright (c) 2016 Paul Jolly <paul@myitcv.org.uk>, all rights reserved.
// Use of this document is governed by a license found in the LICENSE document.

// gg is a wrapper for go generate.
package main // import "myitcv.io/gg"

import (
	"flag"
	"fmt"
	"log"
	"os/exec"
	"sort"
	"strings"
	"sync"
)

const (
	loopLimit      = 10
	typedLoopLimit = loopLimit

	debug = false
)

var (
	fDebug = flag.Bool("debug", false, "debug logging")
)

// When we move gg to be fully-cached based, we will need to load all deps via
// go list so that we can derive a hash of their go files etc. At this point we
// will need https://go-review.googlesource.com/c/go/+/112755 or similar to
// have landed.

var pkgs = make(map[string]*pkg)

func resolve(ip string) *pkg {
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

func loadPkgs(specs []string) pkgSet {
	loaded := make(pkgSet)

	// when we are reloading packages, we will need to reload
	// any rdeps that exist. The specs in this case will already
	// have been fully resolved... so the lookup into pkgs is
	// safe.
	var rdeps []string
	for _, s := range specs {
		if ep, ok := pkgs[s]; ok {
			ep.pending = nil
			for rd := range ep.rdeps {
				if rd.inPkgSpec {
					rd.pending = nil
					rdeps = append(rdeps, rd.ImportPath)
				}
			}
		}
	}
	specs = append(specs, rdeps...)

	var toolsAndOutPkgs []string
	for t := range readPkgs(specs, loaded, false, false) {
		if !t.inPkgSpec {
			toolsAndOutPkgs = append(toolsAndOutPkgs, t.ImportPath)
		}
	}

	// now ensure we have loaded any tools that we not part of the original
	// package spec
	// skip scanning for any directives... these are external tools
	readPkgs(toolsAndOutPkgs, loaded, true, true)

	// populate rdeps
	// TODO we don't need to fully populate this so look to trim at
	// some point later
	for p := range loaded {
		p.pending = new(int)
		if !p.inPkgSpec {
			continue
		}
		for d := range p.deps {
			if d.rdeps == nil {
				d.rdeps = make(pkgSet)
			}
			d.rdeps[p] = true
			if d.pending != nil {
				if *d.pending > 0 {
					*p.pending++
				}
			} else if len(d.toolDeps) > 0 {
				*p.pending++
			}
		}
		for t := range p.toolDeps {
			if t.rdeps == nil {
				t.rdeps = make(pkgSet)
			}
			t.rdeps[p] = true
			*p.pending++
		}
	}

	return loaded
}

func main() {
	setupAndParseFlags("")
	loadConfig()

	var psSlice []*pkg
	ps := loadPkgs(flag.Args())

	possRoots := make(pkgSet)

	for p := range ps {
		psSlice = append(psSlice, p)
		// we set pending on tools
		if p.isTool {
			fmt.Printf("%v %v\n", *p.pending, p)
		}
		if p.isTool && *p.pending == 0 {
			possRoots[p] = true
		}
	}

	sort.Slice(psSlice, func(i, j int) bool {
		return psSlice[i].ImportPath < psSlice[j].ImportPath
	})

	for pr := range possRoots {
		fmt.Printf("Poss root: %v\n", pr)
	}

	var work []*pkg
	for pr := range possRoots {
		work = append(work, pr)
	}

	for len(work) > 0 {
		outPkgs := make(map[*pkg]bool)
		var is, gs []*pkg
		var next []*pkg

	WorkScan:
		for _, w := range work {
			if w.isTool {
				is = append(is, w)
				continue WorkScan
			} else {
				// we are searching for clashes _between_ packages not intra
				// package (because that clash is just fine - no race condition)
				if outPkgs[w] {
					// clash
					goto NoWork
				}
				for _, ods := range w.toolDeps {
					for od := range ods {
						if outPkgs[od] {
							// clash
							goto NoWork
						}
					}
				}
				gs = append(gs, w)
				// no clashes
				outPkgs[w] = true
				for _, ods := range w.toolDeps {
					for od := range ods {
						outPkgs[od] = true
					}
				}
				continue WorkScan
			}

		NoWork:
			next = append(next, w)
		}

		work = next

		var iwg sync.WaitGroup
		var gwg sync.WaitGroup

		// the is (installs) can proceed concurrently, as can the gs (generates),
		// because we know in the case of the latter that their output packages
		// are mutually exclusive
		if len(is) > 0 {
			for _, i := range is {
				i := i
				iwg.Add(1)
				go func() {
					defer func() {
						iwg.Done()
					}()
					cmd := exec.Command("go", "install", i.ImportPath)
					out, err := cmd.CombinedOutput()
					if err != nil {
						fatalf("failed to run %v: %v\n%s", strings.Join(cmd.Args, " "), err, out)
					}
					fmt.Printf("%v\n", strings.Join(cmd.Args, " "))
				}()
			}
		}
		if len(gs) > 0 {
			for _, g := range gs {
				g := g
				gwg.Add(1)
				go func() {
					defer func() {
						gwg.Done()
					}()
					type hashRes map[*pkg]string
					hash := func() hashRes {
						res := make(hashRes)
						res[g] = string(g.hash())
						for _, outPkgMap := range g.toolDeps {
							for op := range outPkgMap {
								res[op] = string(op.hash())
							}
						}
						return res
					}
					i := 0
					pre := hash()

				GoGenerate:
					for {
						i++
						// reload packages
						toReload := []string{g.ImportPath}
						for _, ods := range g.toolDeps {
							for od := range ods {
								toReload = append(toReload, od.ImportPath)
							}
						}

						cmd := exec.Command("go", "generate", g.ImportPath)
						out, err := cmd.CombinedOutput()
						if err != nil {
							fatalf("failed to run %v: %v\n%s", strings.Join(cmd.Args, " "), err, out)
						}

						fmt.Printf("%v (iteration %v)\n", strings.Join(cmd.Args, " "), i)

						loadPkgs(toReload)

						post := hash()
						if len(pre) != len(post) {
							fatalf("bad hashing lengths: pre %v vs post %v?", len(pre), len(post))
						}
						for prep, preh := range pre {
							posth, ok := post[prep]
							if !ok {
								fatalf("bad hashing: pre had %v but post didn't", prep)
							}
							if preh != posth {
								pre = post
								continue GoGenerate
							}
						}
						break
					}
				}()
			}
		}

		iwg.Wait()
		gwg.Wait()

		for _, p := range append(is, gs...) {
			fmt.Printf("are we pending? %v %v\n", *p.pending, p)
			for rd := range p.rdeps {
				*rd.pending--
				fmt.Printf(" - checking rdep (%v) %v -> %v\n", *rd.pending, p, rd)
				if *rd.pending == 0 {
					work = append(work, rd)
				}
			}
		}
	}

	for _, p := range psSlice {
		if p.isTestPkg {
			continue
		}
		fmt.Printf("%#x %v\n", p.hash(), p)
	}

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
