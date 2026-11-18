package discover

import (
	"path/filepath"
	"strings"

	"agentrm/internal/model"
	"agentrm/internal/sys"
)

// Xcode's helpers. After a build, the SourceKit service, the build service and the language
// server can stay large for hours. Xcode (or the editor that runs sourcekit-lsp) starts each
// again when it needs it, so ending an idle one loses nothing but a warm cache.

// XcodeHelperMinMB is the least a helper must hold to be worth listing.
const XcodeHelperMinMB = 1024

// xcodeHelpers are the helpers' executable names, and what each is.
var xcodeHelpers = map[string]string{
	"SourceKitService":     "SourceKit (code completion, indexing)",
	"com.apple.dt.SKAgent": "SourceKit agent",
	"XCBBuildService":      "Xcode build service",
	"SWBBuildService":      "Xcode build service (Swift Build)",
	"sourcekit-lsp":        "Swift language server",
	"clangd":               "C language server",
	"swift-frontend":       "Swift compiler left running",
	// Indexing runs in SourceKitService and in Xcode's own process; Xcode is an app, and is
	// never ended.
}

// xcodeTool says whether an executable is one of Xcode's or a Swift toolchain's.
func xcodeTool(exe string) bool {
	return strings.Contains(exe, "/Xcode") || strings.Contains(exe, ".xctoolchain/") ||
		strings.Contains(exe, "/Library/Developer/")
}

// xcodeHelper is the resource for one of Xcode's helpers, or nil. A compiler counts only once
// its build has gone (its parent is launchd): one under a build service is compiling.
func xcodeHelper(procs map[int]*sys.Proc, p *sys.Proc) *model.Resource {
	base := filepath.Base(p.Exe)
	what := xcodeHelpers[base]
	if what == "" || !xcodeTool(p.Exe) || (base == "swift-frontend" && p.PPID != 1) {
		return nil
	}
	mb := p.RSSMB
	if fp, ok := footprintOf(p.PID); ok && fp > mb {
		mb = fp
	}
	if mb < XcodeHelperMinMB {
		return nil
	}
	r := procRes(p, "xcode", model.RKXcodeHelper, base, what, mb)
	r.Restart = "Xcode starts it again when it needs it"
	return r
}
