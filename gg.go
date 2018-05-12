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
		p.pending = make(map[*pkg]bool)
		if !p.inPkgSpec {
			continue
		}
		for d := range p.deps {
			if d.rdeps == nil {
				d.rdeps = make(pkgSet)
			}
			d.rdeps[p] = true
			if d.pending != nil {
				if len(d.pending) > 0 {
					p.pending[d] = true
				}
			} else if len(d.toolDeps) > 0 {
				p.pending[d] = true
			}
		}
		for t := range p.toolDeps {
			if t.rdeps == nil {
				t.rdeps = make(pkgSet)
			}
			t.rdeps[p] = true
			if t.pending != nil {
				if len(t.pending) > 0 {
					p.pending[t] = true
				}
			} else {
				p.pending[t] = true
			}
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
			fmt.Printf("%v %v\n", len(p.pending), p)
		}
		if p.isTool && len(p.pending) == 0 {
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

				// TODO maybe creating post isn't necessary
				post := make(hashRes)
				post[g] = ""
				for _, outPkgMap := range g.toolDeps {
					for op := range outPkgMap {
						post[op] = ""
					}
				}
				state[g] = &pkgState{
					pre: g.snap(),
					post: post,
				}
			}

		GoGenerate:
			for {
				checkCount := 0
				for g := range state {
					g := g

					if g.pending {
						continue
					}

					if g.pre != g.post {
						g.pending = true
						// fire off work
						go func() {
								cmd := exec.Command("go", "generate", g.ImportPath)
								out, err := cmd.CombinedOutput()
								if err != nil {
									fatalf("failed to run %v: %v\n%s", strings.Join(cmd.Args, " "), err, out)
								}

								fmt.Printf("%v (iteration %v)\n", strings.Join(cmd.Args, " "), i)
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
				case g <- done:
					g.pending = false

					// reload packages
					toReload := []string{g.ImportPath}
					for _, ods := range g.toolDeps {
						for od := range ods {
							toReload = append(toReload, od.ImportPath)
						}
					}

					loadPkgs(toReload)

					state[g].post = g.snap()

						go func() {
							defer func() {
								done <- g
							}()
							i := 0




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

					if !didWork {
						break GoGenerate
					}
				}


		iwg.Wait()

		for _, p := range append(is, gs...) {
			if len(p.pending) > 0 {
				fmt.Printf("%v is still pending on:\n", p)
				for pp := range p.pending {
					fmt.Printf(" + %v\n", pp)
				}
			}
			for rd := range p.rdeps {
				delete(rd.pending, p)
				fmt.Printf(" - checking rdep (%v) %v -> %v\n", len(rd.pending), p, rd)
				if len(rd.pending) == 0 {
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
