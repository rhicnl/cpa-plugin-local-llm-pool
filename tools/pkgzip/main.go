// Command pkgzip builds and checks the plugin's release archives. It is the
// same code path the Makefile and the release workflow use, so what is tested
// locally is what CI publishes.
//
// Usage:
//
//	pkgzip pack      -id <id> -version <v> -goos <os> -goarch <arch> -lib <path> -out <dir>
//	pkgzip checksums -dir <dir>
//	pkgzip verify    -dir <dir> -id <id> -version <v> [-goos <os> -goarch <arch>]
//
// verify without -goos and -goarch checks every archive in the directory.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/rhicnl/cpa-plugin-local-llm-pool/internal/pkgzip"
)

func main() {
	if len(os.Args) < 2 {
		fail(fmt.Errorf("usage: pkgzip <pack|checksums|verify> [flags]"))
	}
	switch command := os.Args[1]; command {
	case "pack":
		runPack(os.Args[2:])
	case "checksums":
		runChecksums(os.Args[2:])
	case "verify":
		runVerify(os.Args[2:])
	default:
		fail(fmt.Errorf("unknown command %q: want pack, checksums or verify", command))
	}
}

func runPack(args []string) {
	flags := flag.NewFlagSet("pack", flag.ExitOnError)
	id := flags.String("id", "", "plugin id")
	version := flags.String("version", "", "release version without the leading v")
	goos := flags.String("goos", "", "target operating system")
	goarch := flags.String("goarch", "", "target architecture")
	lib := flags.String("lib", "", "path to the built dynamic library")
	out := flags.String("out", "dist", "output directory")
	mustParse(flags, args, map[string]*string{"id": id, "version": version, "goos": goos, "goarch": goarch, "lib": lib})

	path, err := pkgzip.Pack(*id, *version, *goos, *goarch, *lib, *out)
	if err != nil {
		fail(err)
	}
	fmt.Printf("packed %s\n", path)
}

func runChecksums(args []string) {
	flags := flag.NewFlagSet("checksums", flag.ExitOnError)
	dir := flags.String("dir", "dist", "directory holding the archives")
	mustParse(flags, args, nil)

	path, err := pkgzip.WriteChecksums(*dir)
	if err != nil {
		fail(err)
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		fail(errRead)
	}
	fmt.Printf("wrote %s\n%s", path, raw)
}

func runVerify(args []string) {
	flags := flag.NewFlagSet("verify", flag.ExitOnError)
	dir := flags.String("dir", "dist", "directory holding the archives")
	id := flags.String("id", "", "plugin id")
	version := flags.String("version", "", "release version without the leading v")
	goos := flags.String("goos", "", "target operating system, empty to verify every archive")
	goarch := flags.String("goarch", "", "target architecture, empty to verify every archive")
	mustParse(flags, args, map[string]*string{"id": id, "version": version})

	var err error
	if *goos == "" || *goarch == "" {
		err = pkgzip.VerifyAll(*dir, *id, *version)
	} else {
		err = pkgzip.Verify(*dir, *id, *version, *goos, *goarch)
	}
	if err != nil {
		fail(err)
	}
}

func mustParse(flags *flag.FlagSet, args []string, required map[string]*string) {
	if err := flags.Parse(args); err != nil {
		fail(err)
	}
	for name, value := range required {
		if *value == "" {
			fail(fmt.Errorf("-%s is required", name))
		}
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "pkgzip: %v\n", err)
	os.Exit(1)
}
