package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os/exec"
	"strings"
)

var (
	pkgInfo = map[string]*Package{}
)

type Package struct {
	Dir          string
	Name         string
	ImportPath   string
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

	testPkg *pkg

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

type pkgSet map[*pkg]bool

func readPkgs(pkgs []string) chan *Package {
	res := make(chan *Package)

	go func() {
		args := []string{"go", "list", "-test", "-f", `{"Dir": "{{.Dir}}", "Name": "{{.Name}}", "ImportPath": "{{.ImportPath}}"{{with .Deps}}, "Deps": ["{{join . "\", \""}}"]{{end}}{{with .GoFiles}}, "GoFiles": ["{{join . "\", \""}}"]{{end}}{{with .TestGoFiles}}, "TestGoFiles": ["{{join . "\", \""}}"]{{end}}{{with .CgoFiles}}, "CgoFiles": ["{{join . "\", \""}}"]{{end}}{{with .XTestGoFiles}}, "XTestGoFiles": ["{{join . "\", \""}}"]{{end}}}`}
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

	return res
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
