package main

import (
	"mcp_patch_file/server"
	"mcp_patch_file/tools/counttokens"
	"mcp_patch_file/tools/filehead"
	"mcp_patch_file/tools/filestats"
	"mcp_patch_file/tools/filesymbols"
	"mcp_patch_file/tools/findcallers"
	"mcp_patch_file/tools/findrelated"
	"mcp_patch_file/tools/findsymbol"
	"mcp_patch_file/tools/gitcontext"
	"mcp_patch_file/tools/gitdiff"
	"mcp_patch_file/tools/grepcontext"
	"mcp_patch_file/tools/grepdead"
	"mcp_patch_file/tools/grepfunc"
	"mcp_patch_file/tools/grepimports"
	"mcp_patch_file/tools/greprefs"
	"mcp_patch_file/tools/grepscope"
	"mcp_patch_file/tools/grepstruct"
	"mcp_patch_file/tools/memory"
	"mcp_patch_file/tools/patchedit"
	"mcp_patch_file/tools/pkgoutline"
	"mcp_patch_file/tools/readsymbol"
	"mcp_patch_file/tools/renamesymbol"
)

func main() {
	s := server.New("patch-file", "0.9.0")
	s.Register(patchedit.Tool, patchedit.Handle)
	s.Register(grepfunc.Tool, grepfunc.Handle)
	s.Register(grepstruct.Tool, grepstruct.Handle)
	s.Register(grepcontext.Tool, grepcontext.Handle)
	s.Register(filehead.Tool, filehead.Handle)
	s.Register(findrelated.Tool, findrelated.Handle)
	s.Register(findsymbol.Tool, findsymbol.Handle)
	s.Register(findcallers.Tool, findcallers.Handle)
	s.Register(memory.Tool, memory.Handle)
	s.Register(filesymbols.Tool, filesymbols.Handle)
	s.Register(filestats.Tool, filestats.Handle)
	s.Register(readsymbol.Tool, readsymbol.Handle)
	s.Register(grepimports.Tool, grepimports.Handle)
	s.Register(greprefs.Tool, greprefs.Handle)
	s.Register(patchedit.BatchTool, patchedit.BatchHandle)
	s.Register(pkgoutline.Tool, pkgoutline.Handle)
	s.Register(gitcontext.Tool, gitcontext.Handle)
	s.Register(counttokens.Tool, counttokens.Handle)
	s.Register(grepdead.Tool, grepdead.Handle)
	s.Register(renamesymbol.Tool, renamesymbol.Handle)
	s.Register(gitdiff.Tool, gitdiff.Handle)
	s.Register(grepscope.Tool, grepscope.Handle)
	s.Run()
}
