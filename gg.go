// Copyright (c) 2016 Paul Jolly <paul@myitcv.org.uk>, all rights reserved.
// Use of this document is governed by a license found in the LICENSE document.

package main

// gg is a wrapper for ``go generate''. More docs to follow

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/myitcv/gogenerate"
)

const (
	untypedLoopLimit = 10
	typedLoopLimit   = 10
)

// All code basically derived from rsc.io/gt

// TODO we effectively read from some files twice... whilst computing stale and scanning
// for directives. These two operations could potentially be collapsed into a single read

func main() {
	log.SetFlags(0)
	log.SetPrefix("")

	flag.Parse()

	loadConfig()

	pkgs := explodePkgList(flag.Args())
	sort.Strings(pkgs)

	readPkgs(pkgs)

	cmds := make(map[string]map[string]struct{})
	pkgs = cmdList(pkgs, cmds)

	if len(cmds) == 0 {
		vvlogln("No packages contain any directives")
		os.Exit(0)
	}

	if *fList {
		allCmds := make(map[string]struct{})

		for _, m := range cmds {
			for k := range m {
				allCmds[k] = struct{}{}
			}
		}
		cs := keySlice(allCmds)
		sort.Strings(cs)
		fmt.Println(strings.Join(cs, "\n"))
		os.Exit(0)
	}

	untypedRunExp := buildGoGenRegex(config.Untyped)
	typedRunExp := buildGoGenRegex(config.Typed)

	diffs := computeStale(pkgs, false)

	typedCount := 1

	var suc, fail []string

	for {
		untypedCount := 1

		if len(pkgs) == 0 {
			vvlogln("No packages contain any directives")
			os.Exit(0)
		}

		// in the first iteration the cmds map-list has already
		// been populated above
		if typedCount != 1 {
			cmds = make(map[string]map[string]struct{})
			_ = cmdList(pkgs, cmds)
		}

		preUntyped := snapHash(diffs)

		// we only delete on the first run...
		if typedCount == 1 {
			rmGenFiles(diffs, config.Untyped)
			rmGenFiles(pkgs, config.Typed)
		}

		for len(diffs) > 0 {
			if untypedCount > untypedLoopLimit {
				fatalf("Exceeded loop limit for untyped go generate cmd: %v\n", untypedRunExp)
			}

			vvlogf("Untyped iteration %v\n", untypedCount)
			goGenerate(diffs, untypedRunExp)
			untypedCount++

			// order is significant here... because the computeStale
			// call does a readPkgs
			prevDiffs := diffs
			diffs = computeStale(prevDiffs, true)
			_ = cmdList(prevDiffs, cmds)
		}

		postUntypedDelta := deltaHash(preUntyped)

		if len(fail) == 0 && len(postUntypedDelta) == 0 {
			vvlogln("The hash delta post untyped phase was nothing; hence no need to move on")
			os.Exit(0)
		}

		// are there any typed commands? If not, we're done
		typedTodo := false

		for c := range cmdMap(cmds) {
			if _, ok := config.typedCmds[c]; ok {
				typedTodo = true
			}
		}

		if !typedTodo {
			vvlogln("No packages contain any typed directives")
			os.Exit(0)
		}

		// TODO work out what to do here when gg is being used in conjunction
		// with gai
		suc, fail = goInstall(pkgs)

		if len(suc) == 0 {
			fatalf("No packages from %v succeeded install; cannot continue\n", pkgs)
		}

		if typedCount > typedLoopLimit {
			fatalf("Exceeded loop limit for typed go generate cmd: %v\n", untypedRunExp)
		}

		vvlogf("Typed iteration %v\n", typedCount)
		goGenerate(suc, typedRunExp)
		typedCount++

		// order is significant here... because the computeStale
		// call does a readPkgs
		diffs = computeStale(suc, true)

		if len(diffs) == 0 && len(fail) == 0 {
			break
		}

		pkgs = append(fail, diffs...)
	}
}

func buildGoGenRegex(parts []string) string {
	escpd := make([]string, len(parts))

	for i := range parts {
		cmd := filepath.Base(parts[i])
		escpd[i] = regexp.QuoteMeta(cmd)
	}

	exp := fmt.Sprintf(gogenerate.GoGeneratePrefix+" (?:%v)(?:$| )", strings.Join(escpd, "|"))

	// aggressively ensure the regexp compiles here... else a call to go generate
	// will be useless
	_, err := regexp.Compile(exp)
	if err != nil {
		fatalf("Could not form valid go generate command: %v\n", err)
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

	xlogln("go ", strings.Join(args, " "))

	out, err := exec.Command("go", args...).CombinedOutput()
	if err != nil {
		fatalf("go generate: %v\n%s", err, out)
	}

	if len(out) > 0 {
		// we always log the output from go generate
		fmt.Print(string(out))
	}
}

func goInstall(pkgs []string) ([]string, []string) {
	fmap := make(map[string]struct{})

	cmds := [][]string{
		append([]string{"install"}, pkgs...),
		append([]string{"test", "-i"}, pkgs...),
	}

	for _, args := range cmds {

		xlogln("go", strings.Join(args, " "))

		out, err := exec.Command("go", args...).CombinedOutput()

		// TODO this is probably a bit brittle... if there is an error we get the list of
		// packages where there is an error and return that

		if err != nil {
			sc := bufio.NewScanner(bytes.NewBuffer(out))
			for sc.Scan() {
				line := sc.Text()

				if strings.HasPrefix(line, "# ") {
					parts := strings.Fields(line)

					if len(parts) != 2 {
						fatalf("could not parse go install output\n%v", string(out))
					}

					fmap[parts[1]] = struct{}{}
				}
			}

			if err := sc.Err(); err != nil {
				fatalf("could not parse go install output\n%v", string(out))
			}
		}

		if len(out) > 0 {
			xlog(string(out))
		}
	}

	var f, s []string

	for _, p := range pkgs {
		if _, ok := fmap[p]; ok {
			f = append(f, p)
		} else {
			s = append(s, p)
		}
	}

	return s, f
}

func cmdList(pNames []string, cmds map[string]map[string]struct{}) []string {
	dirPkgs := make(map[string]struct{})

	for _, pName := range pNames {
		if _, ok := cmds[pName]; !ok {
			cmds[pName] = make(map[string]struct{})
		}

		pkg := pkgInfo[pName]

		var goFiles []string
		goFiles = append(goFiles, pkg.GoFiles...)
		goFiles = append(goFiles, pkg.CgoFiles...)
		goFiles = append(goFiles, pkg.TestGoFiles...)
		goFiles = append(goFiles, pkg.XTestGoFiles...)

		for _, f := range goFiles {

			f = filepath.Join(pkg.Dir, f)

			of, err := os.Open(f)
			if err != nil {
				panic(err)
				// fatalf("calc gen list: %v\n", err)
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
						fatalf("calc gen list: failed to recover from long line: %v\n", err)
					}

					// we recovered...
					continue
				}

				if err == io.EOF {
					break
				}

				if err != nil {
					fatalf("calc gen list: %v\n", err)
				}

				if isGoGenerate(buf) {
					dirPkgs[pName] = struct{}{}

					parts := strings.Fields(string(buf))

					if len(parts) < 2 {
						fatalf("calc gen list: no arguments to direct on line %v in %v\n", line, f)
					}

					cmds[pName][parts[1]] = struct{}{}
				}
			}

			of.Close()
		}
	}

	cm := cmdMap(cmds)

	for v := range cm {

		_, tok := config.typedCmds[v]
		_, uok := config.untypedCmds[v]

		if !tok && !uok {
			log.Fatalln("go generate directive command \"%v\" is not specified as either typed or untyped")
		}
	}

	return keySlice(dirPkgs)
}

func rmGenFiles(pkgs []string, cmds []string) {
	for _, p := range pkgs {
		dir := pkgInfo[p].Dir

		files, err := ioutil.ReadDir(dir)
		if err != nil {
			log.Fatal(err)
		}

	File:
		for _, file := range files {
			if file.IsDir() {
				continue
			}

			fn := file.Name()

			if !strings.HasPrefix(fn, "gen_") {
				continue
			}

			for _, cmd := range cmds {
				if strings.HasSuffix(fn, "."+cmd+".go") {
					fp := filepath.Join(dir, fn)

					vvlogln("Removing ", fp)
					os.Remove(fp)

					continue File
				}
			}
		}

	}
}

func fatalf(format string, args ...interface{}) {
	log.Fatalf(format, args...)
}

func xlogln(args ...interface{}) {
	if *fVVerbose || *fExecute {
		log.Println(args...)
	}
}

func xlog(args ...interface{}) {
	if *fVVerbose || *fExecute {
		log.Print(args...)
	}
}

func vvlogln(args ...interface{}) {
	if *fVVerbose {
		log.Println(args...)
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

func keySlice(m map[string]struct{}) []string {
	res := make([]string, 0, len(m))

	for k := range m {
		res = append(res, k)
	}

	return res
}

// borrowed from cmd/go/generate.go
func isGoGenerate(buf []byte) bool {
	return bytes.HasPrefix(buf, []byte("//go:generate ")) || bytes.HasPrefix(buf, []byte("//go:generate\t"))
}
