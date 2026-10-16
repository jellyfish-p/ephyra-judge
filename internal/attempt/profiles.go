package attempt

import "path"

const (
	profileCPP23 = "cpp23.gcc.linux-amd64.v0"
	profileC17   = "c17.gcc.linux-amd64.v0"
	profileC23   = "c23.gcc.linux-amd64.v0"
)

type profile struct {
	id, languageID, sourceName string
	compileArgs, runArgs       []string
}

// ProfileIDs returns the execution profile IDs lookupProfile knows.
func ProfileIDs() []string {
	return []string{profileCPP23, profileC17, profileC23}
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
