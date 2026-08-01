// Command grepfunc runs the MCP server exposing grep-based code tools.
package main

import (
	"flag"

	"github.com/hedtahr/grepfunc/server"
	"github.com/hedtahr/grepfunc/tools/counttokens"
	"github.com/hedtahr/grepfunc/tools/deletesymbol"

	"github.com/hedtahr/grepfunc/tools/filestats"
	"github.com/hedtahr/grepfunc/tools/filesymbols"
	"github.com/hedtahr/grepfunc/tools/findrelated"
	"github.com/hedtahr/grepfunc/tools/findsymbol"
	"github.com/hedtahr/grepfunc/tools/gitcontext"
	"github.com/hedtahr/grepfunc/tools/grepcontext"
	"github.com/hedtahr/grepfunc/tools/grepdead"
	"github.com/hedtahr/grepfunc/tools/grepfunc"
	"github.com/hedtahr/grepfunc/tools/grepimports"
	"github.com/hedtahr/grepfunc/tools/greprefs"
	"github.com/hedtahr/grepfunc/tools/grepreplace"
	"github.com/hedtahr/grepfunc/tools/grepstruct"
	"github.com/hedtahr/grepfunc/tools/memory"
	"github.com/hedtahr/grepfunc/tools/movesymbol"
	"github.com/hedtahr/grepfunc/tools/multiread"
	"github.com/hedtahr/grepfunc/tools/patchedit"

	"github.com/hedtahr/grepfunc/tools/renamesymbol"
	"github.com/hedtahr/grepfunc/tools/symbolat"
	"github.com/hedtahr/grepfunc/tools/toolstats"
)

func main() {
	projectRoot := flag.String("project-root", "", "override project root directory")

	flag.Parse()

	if *projectRoot != "" {
		server.ProjectRoot = *projectRoot
	}

	srv := server.New("patch-file", "0.9.0")
	srv.Register(patchedit.Tool, patchedit.Handle)
	srv.Register(grepfunc.Tool, grepfunc.Handle)
	srv.Register(grepstruct.Tool, grepstruct.Handle)
	srv.Register(grepcontext.Tool, grepcontext.Handle)

	srv.Register(findrelated.Tool, findrelated.Handle)
	srv.Register(findsymbol.Tool, findsymbol.Handle)
	srv.Register(memory.Tool, memory.Handle)
	srv.Register(filesymbols.Tool, filesymbols.Handle)
	srv.Register(filestats.Tool, filestats.Handle)
	srv.Register(grepimports.Tool, grepimports.Handle)
	srv.Register(greprefs.Tool, greprefs.Handle)
	srv.Register(patchedit.BatchTool, patchedit.BatchHandle)

	srv.Register(gitcontext.GitTool, gitcontext.GitHandle)
	srv.Register(counttokens.Tool, counttokens.Handle)
	srv.Register(grepdead.Tool, grepdead.Handle)
	srv.Register(deletesymbol.Tool, deletesymbol.Handle)
	srv.Register(renamesymbol.Tool, renamesymbol.Handle)
	srv.Register(multiread.Tool, multiread.Handle)
	srv.Register(symbolat.Tool, symbolat.Handle)
	srv.Register(movesymbol.Tool, movesymbol.Handle)
	srv.Register(grepreplace.Tool, grepreplace.Handle)
	srv.Register(toolstats.Tool, toolstats.Handle)
	srv.Run()
}
