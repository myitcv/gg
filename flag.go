package main

import (
	"flag"
)

var (
	fVVerbose = flag.Bool("vv", false, "output commands as they are executed")
	fList     = flag.Bool("l", false, "list go generate directive commands in packages")
	fVerbose  = flag.Bool("v", false, "print the names of packages and files as they are processed")
	fExecute  = flag.Bool("x", false, "print commands as they are executed")
)
