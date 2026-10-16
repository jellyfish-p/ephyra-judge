package attempt

import (
	"context"
	"io"
	"os/exec"
	"path"
	"strings"
	"time"
)

const (
	profileCPP23 = "cpp23.gcc.linux-amd64.v0"
	profileC17   = "c17.gcc.linux-amd64.v0"
	profileC23   = "c23.gcc.linux-amd64.v0"
)

type profile struct {
	id, languageID, sourceName string
	compileArgs, runArgs       []string
}

// ProfileIDs returns the catalog of execution profile IDs. AdvertiseProfiles
// is what a live node should send: only IDs whose compiler passed self-check.
func ProfileIDs() []string {
	return []string{profileCPP23, profileC17, profileC23}
}

// AdvertiseProfiles probes gcc/g++ and returns the subset of ProfileIDs that
// can compile a trivial translation unit with the profile's -std flag.
func AdvertiseProfiles() []string {
	var out []string
	for _, id := range ProfileIDs() {
		p, ok := lookupProfile(id)
		if !ok {
			continue
		}
		if probeProfile(p) {
			out = append(out, id)
		}
	}
	return out
}

func lookupProfile(id string) (profile, bool) {
	switch id {
	case profileCPP23:
		return profile{
			id: id, languageID: "cpp23", sourceName: "main.cpp",
			compileArgs: []string{"g++", "-std=c++23", "-O2", "-pipe", "-o", "program", "main.cpp"},
			runArgs:     []string{"./program"},
		}, true
	case profileC17:
		return profile{
			id: id, languageID: "c17", sourceName: "main.c",
			compileArgs: []string{"gcc", "-std=c17", "-O2", "-pipe", "-o", "program", "main.c"},
			runArgs:     []string{"./program"},
		}, true
	case profileC23:
		return profile{
			id: id, languageID: "c23", sourceName: "main.c",
			compileArgs: []string{"gcc", "-std=c23", "-O2", "-pipe", "-o", "program", "main.c"},
			runArgs:     []string{"./program"},
		}, true
	default:
		return profile{}, false
	}
}

func toolCompileArgs(srcName, outName string) []string {
	return []string{"g++", "-std=c++23", "-O2", "-pipe", "-o", outName, path.Base(srcName)}
}

func probeProfile(p profile) bool {
	if len(p.compileArgs) == 0 {
		return false
	}
	bin, err := exec.LookPath(p.compileArgs[0])
	if err != nil {
		return false
	}
	std := stdFlag(p.compileArgs)
	lang := "c"
	if strings.HasPrefix(p.languageID, "cpp") {
		lang = "c++"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	args := []string{"-fsyntax-only", "-x", lang, "-"}
	if std != "" {
		args = append([]string{std}, args...)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = strings.NewReader("int main(void){return 0;}\n")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run() == nil
}

func stdFlag(args []string) string {
	for _, a := range args {
		if strings.HasPrefix(a, "-std=") {
			return a
		}
	}
	return ""
}
