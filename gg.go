// Copyright (c) 2016 Paul Jolly <paul@myitcv.org.uk>, all rights reserved.
// Use of this document is governed by a license found in the LICENSE document.

// gg is a wrapper for go generate.
package main // import "myitcv.io/gg"

import (
	"flag"
	"fmt"
	"log"
	"os/exec"
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
	loadOrder, toolsAndOutPkgs := readPkgs(specs, false, false)

	// now ensure we have loaded any tools that we not part of the original
	// package spec
	// skip scanning for any directives... these are external tools
	toolLoadOrder, _ := readPkgs(toolsAndOutPkgs, true, true)

	loadOrder = append(toolLoadOrder, loadOrder...)

	loaded := make(pkgSet)
	for _, l := range loadOrder {
		loaded[l] = true
	}

	// populate rdeps
	// TODO we don't need to fully populate this so look to trim at
	// some point later
	for _, p := range loadOrder {
		if p.inPkgSpec {
			p.resetPending()
		}
		p.rdeps = make(pkgSet)
		if !p.inPkgSpec {
			continue
		}
		for d := range p.deps {
			if d.rdeps == nil {
				d.rdeps = make(pkgSet)
			}
			d.rdeps[p] = true
		}
		for t := range p.toolDeps {
			if t.rdeps == nil {
				t.rdeps = make(pkgSet)
			}
			t.rdeps[p] = true
		}
	}

	return loaded
}

func main() {
	setupAndParseFlags("")
	loadConfig()

	ps := loadPkgs(flag.Args())

	possRoots := make(pkgSet)

	for p := range ps {
		if p.isTool && p.ready() {
			possRoots[p] = true
		}
	}

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
		var rem []*pkg

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
			rem = append(rem, w)
		}

		fmt.Printf("is: %#v\n", importPaths(is))
		fmt.Printf("gs: %#v\n", importPaths(gs))
		fmt.Printf("rem: %#v\n", importPaths(rem))

		work = rem

		var iwg sync.WaitGroup

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
			// when we are done with this block of work we need to reload
			// rdeps of the output packages to ensure they are still current
			rdeps := make(pkgSet)

			done := make(chan *pkg)

			type pkgState struct {
				pre     hashRes
				post    hashRes
				count   int
				pending bool
			}

			state := make(map[*pkg]*pkgState)

			for _, g := range gs {
				for rd := range g.rdeps {
					rdeps[rd] = true
				}

				state[g] = &pkgState{
					pre:  g.snap(),
					post: g.zeroSnap(),
				}
			}

			for {
				checkCount := 0
				for g, gs := range state {
					g := g

					// TODO
					// we need to check that we can still proceed, i.e. we haven't "grown"
					// a new dependency that isn't ready

					if gs.pending {
						continue
					}

					if hashEquals, err := gs.pre.equals(gs.post); err != nil {
						fatalf("failed to compare hashes for %v: %v", g, err)
					} else if !hashEquals {
						gs.pre = gs.post
						gs.pending = true
						gs.count++
						// fire off work
						go func() {
							cmd := exec.Command("go", "generate", g.ImportPath)
							out, err := cmd.CombinedOutput()
							if err != nil {
								fatalf("failed to run %v: %v\n%s", strings.Join(cmd.Args, " "), err, out)
							}

							fmt.Printf("%v (iteration %v)\n", strings.Join(cmd.Args, " "), gs.count)
							done <- g
						}()
					} else {
						checkCount++
					}
				}

				if checkCount == len(state) {
					break
				}

				select {
				case g := <-done:
					state[g].pending = false

					// reload packages
					toReload := []string{g.ImportPath}
					for _, ods := range g.toolDeps {
						for od := range ods {
							toReload = append(toReload, od.ImportPath)
						}
					}

					loadPkgs(toReload)

					state[g].post = g.snap()
				}
			}

			// now reload the rdeps
			var toReload []string
			for rd := range rdeps {
				toReload = append(toReload, rd.ImportPath)
			}
			loadPkgs(toReload)
		}

		iwg.Wait()

		for _, p := range append(is, gs...) {
			if !p.ready() {
				for pp := range p.pendingVal {
					fmt.Printf(" + %v\n", pp)
				}
				fatalf("%v is still pending on:\n", p)
			}
			p.donePending(p)
			fmt.Printf("%v marked as complete\n", p)
			for rd := range p.rdeps {
				rd.donePending(p)
				if rd.ready() {
					fmt.Printf("adding work %v\n", rd)
					work = append(work, rd)
				} else {
					fmt.Printf(" - %v not ready (%v)\n", rd, len(rd.pendingVal))
					for rrd := range rd.pendingVal {
						fmt.Printf("   - %v\n", rrd)
					}
				}
			}
		}
	}
}

func importPaths(ps []*pkg) []string {
	var res []string
	for _, p := range ps {
		res = append(res, p.ImportPath)
	}
	return res
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
