// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This program generates a test to verify that the standard arithmetic
// operators properly handle some special cases. The test file should be
// generated with a known working version of go.
// launch with `go run arithBoundaryGen.go` a file called arithBoundary.go
// will be written into the parent directory containing the tests

package main

import (
	"bytes"
	"fmt"
	"go/format"
	"io/ioutil"
	"log"
	"text/template"
)

// used for interpolation in a text template
type tmplData struct {
	Name, Stype, Symbol string
}

// used to work around an issue with the mod symbol being
// interpreted as part of a format string
func (s tmplData) SymFirst() string {
	return string(s.Symbol[0])
}

// ucast casts an unsigned int to the size in s
func ucast(i uint64, s sizedTestData) uint64 {
	switch s.name {
	case "uint32":
		return uint64(uint32(i))
	case "uint16":
		return uint64(uint16(i))
	case "uint8":
		return uint64(uint8(i))
	}
	return i
}

// icast casts a signed int to the size in s
func icast(i int64, s sizedTestData) int64 {
	switch s.name {
	case "int32":
		return int64(int32(i))
	case "int16":
		return int64(int16(i))
	case "int8":
		return int64(int8(i))
	}
	return i
}

type sizedTestData struct {
	name string
	sn   string
	u    []uint64
	i    []int64
}

// values to generate tests. these should include the smallest and largest values, along
// with any other values that might cause issues. we generate n^2 tests for each size to
// cover all cases.
var szs = []sizedTestData{
	sizedTestData{name: "uint64", sn: "64", u: []uint64{0, 1, 4294967296, 0xffffFFFFffffFFFF}},
	sizedTestData{name: "int64", sn: "64", i: []int64{-0x8000000000000000, -0x7FFFFFFFFFFFFFFF,
		-4294967296, -1, 0, 1, 4294967296, 0x7FFFFFFFFFFFFFFE, 0x7FFFFFFFFFFFFFFF}},

	sizedTestData{name: "uint32", sn: "32", u: []uint64{0, 1, 4294967295}},
	sizedTestData{name: "int32", sn: "32", i: []int64{-0x80000000, -0x7FFFFFFF, -1, 0,
		1, 0x7FFFFFFF}},

	sizedTestData{name: "uint16", sn: "16", u: []uint64{0, 1, 65535}},
	sizedTestData{name: "int16", sn: "16", i: []int64{-32768, -32767, -1, 0, 1, 32766, 32767}},

	sizedTestData{name: "uint8", sn: "8", u: []uint64{0, 1, 255}},
	sizedTestData{name: "int8", sn: "8", i: []int64{-128, -127, -1, 0, 1, 126, 127}},
}

type op struct {
	name, symbol string
}

// ops that we will be generating tests for
var ops = []op{op{"add", "+"}, op{"sub", "-"}, op{"div", "/"}, op{"mod", "%%"}, op{"mul", "*"}}

func main() {

	w := new(bytes.Buffer)
	fmt.Fprintf(w, "package main;\n")
	fmt.Fprintf(w, "import \"fmt\"\n")

	for _, sz := range []int{64, 32, 16, 8} {
		fmt.Fprintf(w, "type utd%d struct {\n", sz)
		fmt.Fprintf(w, "  a,b uint%d\n", sz)
		fmt.Fprintf(w, "  add,sub,mul,div,mod uint%d\n", sz)
		fmt.Fprintf(w, "}\n")

		fmt.Fprintf(w, "type itd%d struct {\n", sz)
		fmt.Fprintf(w, "  a,b int%d\n", sz)
		fmt.Fprintf(w, "  add,sub,mul,div,mod int%d\n", sz)
		fmt.Fprintf(w, "}\n")
	}

	// the function being tested
	testFunc, err := template.New("testFunc").Parse(
		`//go:noinline
		func {{.Name}}_{{.Stype}}_ssa(a, b {{.Stype}}) {{.Stype}} {
	return a {{.SymFirst}} b
}
`)
	if err != nil {
		panic(err)
	}

	// generate our functions to be tested
	for _, s := range szs {
		for _, o := range ops {
			fd := tmplData{o.name, s.name, o.symbol}
			err = testFunc.Execute(w, fd)
			if err != nil {
				panic(err)
			}
		}
	}

	// generate the test data
	for _, s := range szs {
		if len(s.u) > 0 {
			fmt.Fprintf(w, "var %s_data []utd%s = []utd%s{", s.name, s.sn, s.sn)
			for _, i := range s.u {
				for _, j := range s.u {
					fmt.Fprintf(w, "utd%s{a: %d, b: %d, add: %d, sub: %d, mul: %d", s.sn, i, j, ucast(i+j, s), ucast(i-j, s), ucast(i*j, s))
					if j != 0 {
						fmt.Fprintf(w, ", div: %d, mod: %d", ucast(i/j, s), ucast(i%j, s))
					}
					fmt.Fprint(w, "},\n")
				}
			}
			fmt.Fprintf(w, "}\n")
		} else {
			// TODO: clean up this duplication
			fmt.Fprintf(w, "var %s_data []itd%s = []itd%s{", s.name, s.sn, s.sn)
			for _, i := range s.i {
				for _, j := range s.i {
					fmt.Fprintf(w, "itd%s{a: %d, b: %d, add: %d, sub: %d, mul: %d", s.sn, i, j, icast(i+j, s), icast(i-j, s), icast(i*j, s))
					if j != 0 {
						fmt.Fprintf(w, ", div: %d, mod: %d", icast(i/j, s), icast(i%j, s))
					}
					fmt.Fprint(w, "},\n")
				}
			}
			fmt.Fprintf(w, "}\n")
		}
	}

	fmt.Fprintf(w, "var failed bool\n\n")
	fmt.Fprintf(w, "func main() {\n\n")

	verify, err := template.New("tst").Parse(
		`if got := {{.Name}}_{{.Stype}}_ssa(v.a, v.b); got != v.{{.Name}} {
       fmt.Printf("{{.Name}}_{{.Stype}} %d{{.Symbol}}%d = %d, wanted %d\n",v.a,v.b,got,v.{{.Name}})
       failed = true
}
`)

	for _, s := range szs {
		fmt.Fprintf(w, "for _, v := range %s_data {\n", s.name)

		for _, o := range ops {
			// avoid generating tests that divide by zero
			if o.name == "div" || o.name == "mod" {
				fmt.Fprint(w, "if v.b != 0 {")
			}

			err = verify.Execute(w, tmplData{o.name, s.name, o.symbol})

			if o.name == "div" || o.name == "mod" {
				fmt.Fprint(w, "\n}\n")
			}

			if err != nil {
				panic(err)
			}

		}
		fmt.Fprint(w, "    }\n")
	}

	fmt.Fprintf(w, `if failed {
        panic("tests failed")
    }
`)
	fmt.Fprintf(w, "}\n")

	// gofmt result
	b := w.Bytes()
	src, err := format.Source(b)
	if err != nil {
		fmt.Printf("%s\n", b)
		panic(err)
	}

	// write to file
	err = ioutil.WriteFile("../arithBoundary.go", src, 0666)
	if err != nil {
		log.Fatalf("can't write output: %v\n", err)
	}
}
