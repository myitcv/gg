package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var (
	pkgInfo = map[string]*Package{}
)

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

	all := strings.Fields(string(out))

	if len(fXPkgs) == 0 {
		return all
	}

	res := make([]string, 0, len(fXPkgs))

All:
	for _, a := range all {
		for _, x := range fXPkgs {
			if strings.HasSuffix(x, "/...") {
				p := strings.TrimSuffix(x, "/...")

				if a == p || strings.HasPrefix(a, p+"/") {
					continue All
				}
			} else {
				if a == x {
					continue All
				}
			}

		}

		res = append(res, a)
	}

	return res
}

func readPkgs(pkgs []string) {
	args := append([]string{"list", "-e", "-json"}, pkgs...)
	out, err := exec.Command("go", args...).CombinedOutput()
	if err != nil {
		log.Fatalf("go list: %v\n%s", err, string(out))
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

func snapHash(pkgs []string) map[string]string {
	prevHashes := make(map[string]string, len(pkgs))
	for _, p := range pkgs {
		v := ""

		if pkg, ok := pkgInfo[p]; ok {
			v = pkg.pkgHash
		}

		prevHashes[p] = v
	}

	return prevHashes
}

func computeStale(pkgs []string, read bool) []string {
	snap := snapHash(pkgs)

	if read {
		readPkgs(pkgs)
	}

	for _, pkg := range pkgs {
		computePkgHash(pkgInfo[pkg])
	}

	return deltaHash(snap)
}

func deltaHash(snap map[string]string) []string {
	var deltas []string

	for p := range snap {
		if snap[p] != pkgInfo[p].pkgHash {
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
