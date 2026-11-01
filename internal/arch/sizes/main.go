// Command sizes prints the lines of the bot's code per package, leaving out
// tests and utilities. With -json it writes them for a later -base, which
// adds the change from that earlier count.
package main

import (
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"slices"

	"github.com/robdevops/signalbot/internal/arch"
)

func main() {
	root := flag.String("root", ".", "module root to measure")
	asJSON := flag.Bool("json", false, "write the counts as JSON")
	base := flag.String("base", "", "JSON from an earlier run, to show the change")
	flag.Parse()
	files, err := arch.Files(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	pkgs := arch.Packages(files)
	if *asJSON {
		json.NewEncoder(os.Stdout).Encode(pkgs)
		return
	}
	old := map[string]int{}
	if *base != "" {
		data, err := os.ReadFile(*base)
		if err == nil {
			err = json.Unmarshal(data, &old)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	dirs := slices.Sorted(maps.Keys(pkgs))
	for d := range old {
		if _, ok := pkgs[d]; !ok {
			dirs = append(dirs, d)
		}
	}
	slices.Sort(dirs)
	fmt.Println("| package | lines | change |\n|---|---:|---:|")
	total, oldTotal := 0, 0
	for _, d := range dirs {
		total, oldTotal = total+pkgs[d], oldTotal+old[d]
		fmt.Printf("| %s | %d | %s |\n", cmp.Or(d, "."), pkgs[d], change(pkgs[d]-old[d], *base != ""))
	}
	fmt.Printf("| **total** | **%d** | %s |\n", total, change(total-oldTotal, *base != ""))
}

func change(d int, show bool) string {
	switch {
	case !show || d == 0:
		return ""
	case d > 0:
		return fmt.Sprintf("+%d", d)
	}
	return fmt.Sprint(d)
}
